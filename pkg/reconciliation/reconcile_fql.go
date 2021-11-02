package reconciliation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/k8ssandra/cass-operator/pkg/httphelper"
	"github.com/k8ssandra/cass-operator/pkg/internal/result"
)

// parseFQLFromConfig parses the DC config field to determine whether FQL should be enabled based on the presence of full_query_logging_options within the cassandra-yaml field.
// To ease integration into the reconciliation process, it returns (shouldFQLBeEnabled, serverMajorVersion, ReconcileResult) where the ReconcileResult may be result.Error() or result.Continue().
func (rc *ReconciliationContext) parseFQLFromConfig() (bool, int64, result.ReconcileResult) {
	dc := rc.GetDatacenter()
	serverMajorVersion, err := strconv.ParseInt(strings.Split(dc.Spec.ServerVersion, ".")[0], 10, 8)
	if err != nil {
		rc.ReqLogger.Error(err, "error parsing server major version. Can't enable full query logging without knowing this")
		return false, serverMajorVersion, result.Error(err)
	}
	shouldFQLBeEnabled := false
	if dc.Spec.Config != nil {
		var dcConfig map[string]interface{}
		if err := json.Unmarshal(dc.Spec.Config, &dcConfig); err != nil {
			rc.ReqLogger.Error(err, "error unmarshalling DC config JSON")
			return false, serverMajorVersion, result.Error(err)
		}
		casYaml, found := dcConfig["cassandra-yaml"]
		if !found {
			return false, serverMajorVersion, result.Continue()
		}
		casYamlMap, ok := casYaml.(map[string]interface{})
		if !ok {
			err := fmt.Errorf("type casting error")
			rc.ReqLogger.Error(err, "couldn't cast cassandra-yaml value from config to map[string]interface{}")
			return false, serverMajorVersion, result.Error(err)
		}
		if _, found := casYamlMap["full_query_logging_options"]; found {
			if serverMajorVersion < 4 || dc.Spec.ServerType != "cassandra" {
				err := fmt.Errorf("full query logging only supported on OSS Cassandra 4x+")
				rc.ReqLogger.Error(err, "full_query_logging_options is defined in Cassandra config, it is not supported on the version of Cassandra you are running")
				return false, serverMajorVersion, result.Error(err)
			}
			rc.ReqLogger.Info("full_query_logging_options is defined in Cassandra config, we will try to enable it via the management API")
			shouldFQLBeEnabled = true
		}
	}
	if dc.Spec.ServerType != "cassandra" {
		// DSE does not support FQL
		return false, 0, result.Continue()
	}
	return shouldFQLBeEnabled, serverMajorVersion, result.Continue()
}

// SetFullQueryLogging sets FQL enabled or disabled based on the `enableFQL` parameter, and takes serverMajorVersion for additional validation.
// It calls the NodeMgmtClient which calls the Cassandra management API and returns a result.ReconcileResult.
func (rc *ReconciliationContext) SetFullQueryLogging(enableFQL bool, serverMajorVersion int64) result.ReconcileResult {
	// This only checks if Cassandra serverMajorVersion is >= 4, DSE returns 0. No DSE version supports this
	if serverMajorVersion >= 4 {
		rc.ReqLogger.Info("setting FQL as server major version is ", "serverMajorVersion", serverMajorVersion)
		podList, err := rc.listPods(rc.Datacenter.GetClusterLabels())
		if err != nil {
			rc.ReqLogger.Error(err, "error listing all pods in the cluster to progress full query logging reconciliation")
			return result.RequeueSoon(2)
		}
		for _, podPtr := range PodPtrsFromPodList(podList) {
			features, err := rc.NodeMgmtClient.FeatureSet(podPtr)
			if err != nil {
				rc.ReqLogger.Error(err, "failed to verify featureset for FQL support")
				return result.RequeueSoon(2)
			}
			if !features.Supports(httphelper.FullQuerySupport) {
				if enableFQL {
					err := errors.New("FQL should be enabled but we cannot verify if FQL is supported by mgmt api")
					return result.Error(err)
				}
				// FQL support not available in mgmt API but user is not requesting it - continue.
				return result.Continue()
			}
			fqlEnabledForPod, err := rc.NodeMgmtClient.CallIsFullQueryLogEnabledEndpoint(podPtr)
			if err != nil {
				rc.ReqLogger.Error(err, "can't get whether query logging enabled for pod ", "podName", podPtr.Name)
				return result.RequeueSoon(2)
			}
			rc.ReqLogger.Info("full query logging status:", "isEnabled", fqlEnabledForPod, "shouldBeEnabled", enableFQL)
			if fqlEnabledForPod != enableFQL {
				rc.ReqLogger.Info("Setting full query logging on ", "podIP", podPtr.Status.PodIP, "podName", podPtr.Name, "fqlDesiredState", enableFQL)
				err := rc.NodeMgmtClient.CallSetFullQueryLog(podPtr, enableFQL)
				if err != nil {
					rc.ReqLogger.Error(err, "couldn't enable full query logging on ", "podIP", podPtr.Status.PodIP, "podName", podPtr.Name)
					return result.RequeueSoon(2)
				}
			}
		}
	}
	return result.Continue()
}
