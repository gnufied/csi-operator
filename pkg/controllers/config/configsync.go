package config

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/openshift/openstack-cinder-csi-driver-operator/pkg/util"
	ini "gopkg.in/ini.v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// This ConfigSyncController translates the ConfigMap provided by the user
// containing configuration information for the Cinder CSI driver.
type ConfigSyncController struct {
	operatorClient       v1helpers.OperatorClient
	kubeClient           kubernetes.Interface
	configMapLister      corelisters.ConfigMapLister
	infrastructureLister configv1listers.InfrastructureLister
	eventRecorder        events.Recorder
}

const (
	sourceConfigKey   = "config"
	targetConfigKey   = "cloud.conf"
	enableTopologyKey = "enable_topology"

	infrastructureResourceName = "cluster"
)

func NewConfigSyncController(
	operatorClient v1helpers.OperatorClient,
	kubeClient kubernetes.Interface,
	informers v1helpers.KubeInformersForNamespaces,
	configInformers configinformers.SharedInformerFactory,
	resyncInterval time.Duration,
	eventRecorder events.Recorder) factory.Controller {

	// Read configmap from user-managed namespace and save the translated one
	// to the operator namespace
	configMapInformer := informers.InformersFor(util.OpenShiftConfigNamespace)
	c := &ConfigSyncController{
		operatorClient:       operatorClient,
		kubeClient:           kubeClient,
		configMapLister:      configMapInformer.Core().V1().ConfigMaps().Lister(),
		infrastructureLister: configInformers.Config().V1().Infrastructures().Lister(),
		eventRecorder:        eventRecorder.WithComponentSuffix("ConfigSync"),
	}
	return factory.New().WithSync(c.sync).ResyncEvery(resyncInterval).WithSyncDegradedOnError(operatorClient).WithInformers(
		operatorClient.Informer(),
		configMapInformer.Core().V1().ConfigMaps().Informer(),
	).ToController("ConfigSync", eventRecorder)
}

func (c *ConfigSyncController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	var err error

	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}
	if opSpec.ManagementState != operatorv1.Managed {
		return nil
	}

	enableTopologyFeature, err := enableTopologyFeature()
	if err != nil {
		return err
	}

	infra, err := c.infrastructureLister.Get(infrastructureResourceName)
	if err != nil {
		return err
	}

	var sourceConfig *v1.ConfigMap

	// First, we try to retrieve from the Cinder CSI-specific config map
	sourceConfig, err = c.configMapLister.ConfigMaps(util.OpenShiftConfigNamespace).Get("cinder-csi-config")
	if err != nil {
		// Failing that, we attempt to retrieve from the cloud provider-specific config map
		if errors.IsNotFound(err) {
			sourceConfig, err = c.configMapLister.ConfigMaps(util.OpenShiftConfigNamespace).Get(infra.Spec.CloudConfig.Name)
			if err != nil {
				if errors.IsNotFound(err) {
					// TODO: report error after some while?
					klog.V(2).Infof("Waiting for config map %s from %s", infra.Spec.CloudConfig.Name, util.OpenShiftConfigNamespace)
					return nil
				}
				return err
			}
		} else {
			return err
		}
	}

	targetConfig, err := translateConfigMap(sourceConfig, enableTopologyFeature)
	if err != nil {
		return err
	}

	_, _, err = resourceapply.ApplyConfigMap(ctx, c.kubeClient.CoreV1(), c.eventRecorder, targetConfig)
	if err != nil {
		return err
	}
	return nil
}

func translateConfigMap(cloudConfig *v1.ConfigMap, enableTopologyFeature bool) (*v1.ConfigMap, error) {
	// Process the cloud configuration
	content, ok := cloudConfig.Data[sourceConfigKey]
	if !ok {
		return nil, fmt.Errorf("OpenStack config map did not contain key %s", sourceConfigKey)
	}

	cfg, err := ini.Load([]byte(content))
	if err != nil {
		return nil, fmt.Errorf("failed to read the cloud.conf: %w", err)
	}

	// Set the static, must-have keys in the '[Global]' section. If these are
	// already set by the user then tough luck
	global, _ := cfg.GetSection("Global")
	if global != nil {
		klog.Infof("[Global] section found; dropping any legacy settings...")
		// Use a slice to preserve keys order
		for _, o := range []struct{ k, v string }{
			{"secret-name", "openstack-credentials"},
			{"secret-namespace", "kube-system"},
			{"kubeconfig-path", ""},
		} {
			if global.Key(o.k).String() != o.v {
				return nil, fmt.Errorf("'[Global] %s' is set to a non-default value", o.k)
			}
			global.DeleteKey(o.k)
		}
	} else {
		// This probably isn't common but at least handling this allows us to
		// recover gracefully
		global, err = cfg.NewSection("Global")
		if err != nil {
			return nil, fmt.Errorf("failed to modify the provided configuration: %w", err)
		}
	}
	// Use a slice to preserve keys order
	for _, o := range []struct{ k, v string }{
		{"use-clouds", "true"},
		{"clouds-file", "/etc/kubernetes/secret/clouds.yaml"},
		{"cloud", "openstack"},
	} {
		_, err = global.NewKey(o.k, o.v)
		if err != nil {
			return nil, fmt.Errorf("failed to modify the provided configuration: %w", err)
		}
	}

	// Now, modify the '[BlockStorage]' section as necessary
	blockStorage, _ := cfg.GetSection("BlockStorage")
	if blockStorage != nil {
		klog.Infof("[BlockStorage] section found; dropping any legacy settings...")
		// Remove the legacy keys, once we ensure they're not overridden
		if key, _ := blockStorage.GetKey("trust-device-path"); key != nil {
			blockStorage.DeleteKey("trust-device-path")
		}

		// If that was the only key, remove the section also
		if len(blockStorage.KeyStrings()) == 0 {
			cfg.DeleteSection("BlockStorage")
		}
	}

	// Generate our shiny new config map to save into the operator's namespace
	var buf bytes.Buffer

	_, err = cfg.WriteTo(&buf)
	if err != nil {
		return nil, fmt.Errorf("failed to modify the provided configuration: %w", err)
	}

	// Process the topology feature flag
	enableTopologyValue, ok := cloudConfig.Data[enableTopologyKey]
	if ok {
		// use the user-configured value if provided...
		klog.Infof("%s configuration found; using user-provided configuration...", enableTopologyKey)
	} else {
		// ...but fallback to the automatic configuration if not
		enableTopologyValue = strconv.FormatBool(enableTopologyFeature)
	}

	config := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.CinderConfigName,
			Namespace: util.DefaultNamespace,
		},
		Data: map[string]string{
			targetConfigKey:   buf.String(),
			enableTopologyKey: enableTopologyValue,
		},
	}

	return &config, nil
}
