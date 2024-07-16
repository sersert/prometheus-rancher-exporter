package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sersert/prometheus-rancher-exporter/measure"
	"github.com/prometheus/client_golang/prometheus"
)

// Data is used to store data from all the relevant endpoints in the API
type Data struct {
	Data []struct {
		HealthState string            `json:"healthState"`
		Name        string            `json:"name"`
		State       string            `json:"state"`
		System      bool              `json:"system"`
		Scale       int               `json:"scale"`
		HostName    string            `json:"hostname"`
		ID          string            `json:"id"`
		StackID     string            `json:"stackId"`
		EnvID       string            `json:"environmentId"`
		BaseType    string            `json:"basetype"`
		Type        string            `json:"type"`
		AgentState  string            `json:"agentState"`
		Labels      map[string]string `json:"labels"`
		ClusterID   string            `json:"clusterId"`
		NodeName    string            `json:"nodeName"`
		// HostInfo for hosts
		HostInfo *HostInfo `json:"info"`
		// LaunchConfig for services
		LaunchConfig *LaunchConfig `json:"launchConfig"`
		// ComponentStatuses for clusters
		ComponentStatuses []*ComponentStatuses `json:"componentStatuses"`
	} `json:"data"`
}

type HostInfo struct {
	CPUInfo struct {
		Count int `json:"count"`
	} `json:"cpuInfo"`
	MemoryInfo struct {
		MemTotal int `json:"memTotal"`
		MemFree  int `json:"memFree"`
	} `json:"memoryInfo"`
	DiskInfo struct {
		MountPoints map[string]MountPoint `json:"mountPoints"`
	} `json:"diskInfo"`
}

type MountPoint struct {
	Total int `json:"total"`
	Used  int `json:"used"`
}

type LaunchConfig struct {
	Labels map[string]string `json:"labels"`
}

type ComponentStatuses struct {
	Conditions []*Condition `json:"conditions"`
	Name       string       `json:"name"`
}

type Condition struct {
	Status string `json:"status"`
}

// processMetrics - Collects the data from the API, returns data object
func (e *Exporter) processMetrics(data *Data, endpoint string, hideSys bool, ch chan<- prometheus.Metric) error {
	var filteredLabels map[string]string

	// Metrics - range through the data object
	for _, x := range data.Data {
		// If system services have been ignored, the loop simply skips them
		if hideSys && x.System {
			continue
		}

		// Checks the metric is of the expected type
		dataType := x.BaseType
		if dataType == "" {
			dataType = x.Type
		}
		if !checkMetric(endpoint, dataType) {
			continue
		}

		log.Debugf("Processing metrics for %s", endpoint)

		if endpoint == "hosts" {
			filteredLabels = e.allowedLabels(x.Labels)
			var s = x.HostName
			if x.Name != "" {
				s = x.Name
			}
			e.setHostStateMetrics(s, x.State, x.AgentState, filteredLabels)
			if x.HostInfo != nil {
				e.setHostInfoMetrics(s, x.HostInfo, filteredLabels)
			}
		} else if endpoint == "stacks" {
			// Used to create a map of stackID and stackName
			// Later used as a dimension in service metrics
			stackRef = storeStackRef(x.ID, x.Name)

			e.setStackMetrics(x.Name, x.State, x.HealthState, strconv.FormatBool(x.System))
		} else if endpoint == "services" {
			// Retrieves the stack Name from the previous values stored.
			var stackName = retrieveStackRef(x.StackID)

			if stackName == "unknown" {
				log.Warnf("Failed to obtain stack_name for %s from the API", x.Name)
			}

			if x.LaunchConfig != nil && len(x.LaunchConfig.Labels) > 0 {
				filteredLabels = e.allowedLabels(x.LaunchConfig.Labels)
			}

			e.setServiceMetrics(x.Name, stackName, x.State, x.HealthState, x.Scale, filteredLabels)
		} else if endpoint == "clusters" {
			clusterRef = storeClusterRef(x.ID, x.Name)
			e.setClusterMetrics(x.Name, x.State, x.ComponentStatuses)
		} else if endpoint == "nodes" {
			// Retrieves the cluster Name from the previous values stored.
			var clusterName = retrieveClusterRef(x.ClusterID)

			if clusterName == "unknown" {
				log.Warnf("Failed to obtain cluster_name for %s from the API", x.NodeName)
			}

			e.setNodeMetrics(x.NodeName, x.State, clusterName)
		}
	}

	return nil
}

// gatherData - Collects the data from thw API, invokes functions to transform that data into metrics
func (e *Exporter) gatherData(rancherURL string, resourceLimit string, accessKey string, secretKey string, endpoint string, ch chan<- prometheus.Metric) (*Data, error) {
	// Return the correct URL path
	url := setEndpoint(rancherURL, endpoint, resourceLimit)

	// Create new data slice from Struct
	var data = new(Data)

	// Scrape EndPoint for JSON Data
	err := getJSON(url, accessKey, secretKey, &data)
	if err != nil {
		log.Error("Error getting JSON from endpoint ", endpoint)
		return nil, err
	}
	log.Debugf("JSON Fetched for: "+endpoint+": %+v", data)

	return data, err
}

func (e *Exporter) allowedLabels(labels map[string]string) map[string]string {
	result := make(map[string]string)
	for name, val := range labels {
		if e.labelsFilter.MatchString(name) {
			result[name] = val
		}
	}
	return result
}

// getJSON return json from server, return the formatted JSON
func getJSON(url string, accessKey string, secretKey string, target interface{}) error {
	start := time.Now()

	// Counter for internal exporter metrics
	measure.FunctionCountTotal.With(prometheus.Labels{"pkg": "main", "fnc": "getJSON"}).Inc()

	log.Info("Scraping: ", url)

	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)

	if err != nil {
		log.Error("Error Collecting JSON from API: ", err)
	}

	req.SetBasicAuth(accessKey, secretKey)
	resp, err := client.Do(req)

	if err != nil {
		log.Error("Error Collecting JSON from API: ", err)
	}

	if resp.StatusCode != 200 {
		log.Error("Error Collecting JSON from API: ", resp.Status)
	}

	respFormatted := json.NewDecoder(resp.Body).Decode(target)

	// Timings recorded as part of internal metrics
	elapsed := float64((time.Since(start)) / time.Microsecond)
	measure.FunctionDurations.WithLabelValues("main", "getJSON").Observe(elapsed)

	// Close the response body, the underlying Transport should then close the connection.
	resp.Body.Close()

	// return formatted JSON
	return respFormatted
}

// setEndpoint - Determines the correct URL endpoint to use, gives us backwards compatibility
func setEndpoint(rancherURL string, component string, resourceLimit string) string {
	var endpoint string

	endpoint = (rancherURL + "/" + component + "/" + "?limit=" + resourceLimit)
	endpoint = strings.Replace(endpoint, "v1", "v2-beta", 1)

	return endpoint
}

// storeStackRef stores the stackID and stack name for use as a label elsewhere
func storeStackRef(stackID string, stackName string) map[string]string {
	stackRef[stackID] = stackName

	return stackRef
}

// retrieveStackRef returns the stack name, when sending the stackID
func retrieveStackRef(stackID string) string {
	for key, value := range stackRef {
		if stackID == "" {
			return "unknown"
		} else if stackID == key {
			log.Debugf("StackRef - Key is %s, Value is %s StackID is %s", key, value, stackID)
			return value
		}
	}
	// returns unknown if no match was found
	return "unknown"
}

// storeClusterRef stores the clusterID and cluster name for use as a label elsewhere
func storeClusterRef(clusterID string, clusterName string) map[string]string {
	clusterRef[clusterID] = clusterName

	return clusterRef
}

// retrieveClusterRef returns the cluster name, when sending the clusterID
func retrieveClusterRef(clusterID string) string {
	for key, value := range clusterRef {
		if clusterID == "" {
			return "unknown"
		} else if clusterID == key {
			log.Debugf("ClusterRef - Key is %s, Value is %s ClusterID is %s", key, value, clusterID)
			return value
		}
	}
	// returns unknown if no match was found
	return "unknown"
}
