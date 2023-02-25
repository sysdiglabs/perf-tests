/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sysdig

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/perf-tests/clusterloader2/pkg/config"
	"k8s.io/perf-tests/clusterloader2/pkg/framework"
	"k8s.io/perf-tests/clusterloader2/pkg/framework/client"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	measurementutil "k8s.io/perf-tests/clusterloader2/pkg/measurement/util"
	"k8s.io/perf-tests/clusterloader2/pkg/provider"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	// sysdigAgentMeasurementName indicates the measurement name
	sysdigAgentMeasurementName = "SysdigAgent"

	defaultRolloutTimeout = 10 * time.Minute
)

const (
	agentSAFilePath                      = "manifests/sysdig-agent-serviceaccount.yaml"
	agentClusterRoleFilePath             = "manifests/sysdig-agent-clusterrole.yaml"
	agentClusterRoleBindingFilePath      = "manifests/sysdig-agent-clusterrolebinding.yaml"
	agentSecretFilePath                  = "manifests/sysdig-agent-secret.yaml"
	agentConfigMapFilePath               = "manifests/sysdig-agent-configmap.yaml"
	agentDaemonsetFilePath               = "manifests/sysdig-agent-daemonset.yaml"
	kubecollectConfigMapManifestFilePath = "manifests/kubecollect-configMap.yaml"

	sysdigAgentNs = "sysdig-agent-cl2"
)

var (
	cmGvk = schema.GroupVersionKind{
		Group:   "",
		Kind:    "ConfigMap",
		Version: "v1",
	}
	secretGvk = schema.GroupVersionKind{
		Group:   "",
		Kind:    "Secret",
		Version: "v1",
	}
	daemonsetGvk = schema.GroupVersionKind{
		Group:   "apps",
		Kind:    "DaemonSet",
		Version: "v1",
	}
	saGvk = schema.GroupVersionKind{
		Group:   "",
		Kind:    "ServiceAccount",
		Version: "v1",
	}
	clusterRoleGvk = schema.GroupVersionKind{
		Group:   "rbac.authorization.k8s.io",
		Kind:    "ClusterRole",
		Version: "v1",
	}
	clusterRoleBindingGvk = schema.GroupVersionKind{
		Group:   "rbac.authorization.k8s.io",
		Kind:    "ClusterRoleBinding",
		Version: "v1",
	}
	//go:embed manifests
	manifestsFS embed.FS
)

func init() {
	klog.Info("Registering Sysdig Agent Measurement")
	if err := measurement.Register(sysdigAgentMeasurementName, createSysdigAgentMeasurement); err != nil {
		klog.Fatalf("Cannot register %s: %v", sysdigAgentMeasurementName, err)
	}
}

func createSysdigAgentMeasurement() measurement.Measurement {
	return &sysdigAgentMeasurement{}
}

type sysdigAgentMeasurement struct {
	k8sClient         kubernetes.Interface
	dynamicClient     dynamic.Interface
	resourceInterface dynamic.ResourceInterface
	framework         *framework.Framework

	selector          *util.ObjectSelector
	desiredAgentCount int
	rolloutTimeout    time.Duration

	startTime time.Time
	duration  time.Duration
}

func (sam *sysdigAgentMeasurement) Execute(measurementConfig *measurement.Config) ([]measurement.Summary, error) {
	action, err := util.GetString(measurementConfig.Params, "action")
	if err != nil {
		return nil, err
	}
	switch action {
	case "start":
		return nil, sam.start(measurementConfig)
	case "waitForRollout":
		return nil, sam.waitAgentRollout()
	case "gather":
		result := measurementutil.PerfData{
			Version: "v1",
			DataItems: []measurementutil.DataItem{{
				Unit:   "s",
				Labels: map[string]string{"test": "phases"},
				Data:   make(map[string]float64)}}}

		result.DataItems[0].Data["AgentBootstrapTime"] = sam.duration.Seconds()
		content, err := util.PrettyPrintJSON(result)
		if err != nil {
			return nil, err
		}
		summary := measurement.CreateSummary(sysdigAgentMeasurementName, "json", content)
		return []measurement.Summary{summary}, nil
	default:
		return nil, fmt.Errorf("unknown action: %v", action)
	}
}

func (sam *sysdigAgentMeasurement) start(measurementConfig *measurement.Config) error {
	if err := sam.initialize(measurementConfig); err != nil {
		return err
	}

	// In order to trigget the kubecollect containers we apply the
	// configuration.
	mapping, errList := config.GetMapping(measurementConfig.ClusterLoaderConfig, nil)
	if errList != nil {
		return errList
	}
	klog.Infof("sysdig config mapping: %v", mapping)
	switch measurementConfig.CloudProvider.Name() {
	case provider.KubemarkName, provider.GKEKubemarkName:
		if err := sam.framework.ApplyTemplatedManifests(manifestsFS, kubecollectConfigMapManifestFilePath, mapping); err != nil {
			return fmt.Errorf("error while creating config: %v", err)
		}
	case provider.GCEName, provider.AWSName:
		if err := client.CreateNamespace(sam.k8sClient, sysdigAgentNs); err != nil {
			klog.Warningf("failed to create namespace %s: %w", sysdigAgentNs, err)
		}
		if err := sam.framework.ApplyTemplatedManifests(manifestsFS, agentSAFilePath, mapping); err != nil {
			return fmt.Errorf("error while creating service account: %v", err)
		}
		if err := sam.framework.ApplyTemplatedManifests(manifestsFS, agentSecretFilePath, mapping); err != nil {
			return fmt.Errorf("error while creating secret: %v", err)
		}
		if err := sam.framework.ApplyTemplatedManifests(manifestsFS, agentConfigMapFilePath, mapping); err != nil {
			return fmt.Errorf("error while creating config map: %v", err)
		}
		if err := sam.framework.ApplyTemplatedManifests(manifestsFS, agentClusterRoleFilePath, mapping); err != nil {
			return fmt.Errorf("error while creating cluster role: %v", err)
		}
		if err := sam.framework.ApplyTemplatedManifests(manifestsFS, agentClusterRoleBindingFilePath, mapping); err != nil {
			return fmt.Errorf("error while creating cluster role binding: %v", err)
		}
		if err := sam.framework.ApplyTemplatedManifests(manifestsFS, agentDaemonsetFilePath, mapping); err != nil {
			return fmt.Errorf("error while creating daemonset: %v", err)
		}
	default:
		return fmt.Errorf("unsupported provider for sysdig agent %s", measurementConfig.CloudProvider)
	}
	sam.startTime = time.Now()
	return nil
}

func (sam *sysdigAgentMeasurement) initialize(measurementConfig *measurement.Config) error {
	var err error
	sam.desiredAgentCount = measurementConfig.ClusterFramework.GetClusterConfig().Nodes
	klog.Infof("desired agent count: %d", sam.desiredAgentCount)
	sam.selector = util.NewObjectSelector()
	sam.rolloutTimeout, err = util.GetDurationOrDefault(measurementConfig.Params, "rolloutTimeout", defaultRolloutTimeout)
	if err != nil {
		return err
	}
	switch measurementConfig.CloudProvider.Name() {
	case provider.KubemarkName, provider.GKEKubemarkName:
		if measurementConfig.PrometheusFramework == nil {
			return errors.New("PrometheusFramework is not enabled")
		}
		sam.selector.Namespace, err = util.GetStringOrDefault(measurementConfig.Params, "namespace", "kubemark")
		if err != nil {
			return err
		}
		sam.selector.LabelSelector, err = util.GetStringOrDefault(measurementConfig.Params, "labelSelector", "name=hollow-node")
		if err != nil {
			return err
		}
		sam.framework = measurementConfig.PrometheusFramework
		sam.k8sClient = measurementConfig.PrometheusFramework.GetClientSets().GetClient()
		sam.dynamicClient = measurementConfig.PrometheusFramework.GetDynamicClients().GetClient()
	case provider.GCEName, provider.AWSName:
		sam.selector.Namespace, err = util.GetStringOrDefault(measurementConfig.Params, "namespace", sysdigAgentNs)
		if err != nil {
			return err
		}
		sam.selector.LabelSelector, err = util.GetStringOrDefault(measurementConfig.Params, "labelSelector", "app.kubernetes.io/name=sysdig-agent")
		if err != nil {
			return err
		}
		sam.k8sClient = measurementConfig.ClusterFramework.GetClientSets().GetClient()
		sam.framework = measurementConfig.ClusterFramework
		sam.dynamicClient = measurementConfig.ClusterFramework.GetDynamicClients().GetClient()
	default:
		return fmt.Errorf("unsupported provider for sysdig agent %s", measurementConfig.CloudProvider)
	}
	return nil
}

func (sam *sysdigAgentMeasurement) waitAgentRollout() error {
	defer func() {
		duration := time.Since(sam.startTime)
		klog.V(0).Infof("%s rollout duration: %v", sam, duration)
		sam.duration = duration
	}()
	if sam.framework == nil {
		return errors.New("sysdig agent not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), sam.rolloutTimeout)
	defer cancel()
	options := &measurementutil.WaitForPodOptions{
		DesiredPodCount:     func() int { return sam.desiredAgentCount },
		CallerName:          sam.String(),
		WaitForPodsInterval: 2 * time.Second,
	}
	podStore, err := measurementutil.NewPodStore(sam.k8sClient, sam.selector)
	if err != nil {
		return err
	}
	_, err = measurementutil.WaitForPods(ctx, podStore, options)
	return err
}

func (sam *sysdigAgentMeasurement) Dispose() {
	if sam.framework == nil {
		klog.Warning("sydgig agent not initialized")
		return
	}
	if err := sam.framework.DeleteObject(cmGvk, "kubemark", "config"); err != nil {
		klog.Warningf("Failed to deleted kubecollect ConfigMap: %v", err)
	}
	if err := sam.framework.DeleteObject(cmGvk, sysdigAgentNs, "sysdig-agent"); err != nil {
		klog.Warningf("Failed to deleted agent ConfigMap: %v", err)
	}
	if err := sam.framework.DeleteObject(secretGvk, sysdigAgentNs, "sysdig-agent"); err != nil {
		klog.Warningf("Failed to deleted agent Secret: %v", err)
	}
	if err := sam.framework.DeleteObject(daemonsetGvk, sysdigAgentNs, "sysdig-agent"); err != nil {
		klog.Warningf("Failed to deleted agent Daemonset: %v", err)
	}
	if err := sam.framework.DeleteObject(saGvk, sysdigAgentNs, "sysdig-agent"); err != nil {
		klog.Warningf("Failed to deleted agent ServiceAccount: %v", err)
	}
	if err := sam.framework.DeleteObject(saGvk, sysdigAgentNs, "sysdig-agent"); err != nil {
		klog.Warningf("Failed to deleted agent ServiceAccount: %v", err)
	}
	if err := sam.framework.DeleteObject(clusterRoleBindingGvk, "", "sysdig-agent-cl2"); err != nil {
		klog.Warningf("Failed to deleted agent ClusterRoleBinding: %v", err)
	}
	if err := sam.framework.DeleteObject(clusterRoleGvk, "", "sysdig-agent-cl2"); err != nil {
		klog.Warningf("Failed to deleted agent ClusterRoleBinding: %v", err)
	}
}

func (*sysdigAgentMeasurement) String() string {
	return sysdigAgentMeasurementName
}
