/*Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package skyring

import (
	"encoding/json"
	"fmt"

	"github.com/gorilla/mux"
	"github.com/skyrings/skyring-common/conf"
	"github.com/skyrings/skyring-common/db"
	"github.com/skyrings/skyring-common/models"
	"github.com/skyrings/skyring-common/monitoring"
	"github.com/skyrings/skyring-common/tools/logger"
	"github.com/skyrings/skyring-common/tools/schedule"
	"github.com/skyrings/skyring-common/tools/task"
	"github.com/skyrings/skyring-common/tools/uuid"
	"github.com/skyrings/skyring-common/utils"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"

	"io"
	"io/ioutil"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

func getEntityName(entity_type string, entity_id uuid.UUID, parentId *uuid.UUID) (string, error) {
	switch entity_type {
	case monitoring.NODE:
		entity, entityFetchErr := GetNode(entity_id)
		if entityFetchErr != nil {
			return "", fmt.Errorf("Unknown %v with id %v.Err %v", entity_type, entity_id, entityFetchErr)
		}
		return entity.Hostname, nil
	case models.CLUSTER:
		entity, entityFetchErr := GetCluster(&entity_id)
		if entityFetchErr != nil {
			return "", fmt.Errorf("Unknown %v with id %v.Err %v", entity_type, entity_id, entityFetchErr)
		}
		return entity.Name, nil
	case monitoring.SLU:
		entity, entityFetchErr := GetSLU(parentId, entity_id)
		if entityFetchErr != nil {
			return "", fmt.Errorf("%v not a valid id of %v.Err %v", entity_id, entity_type, entityFetchErr)
		}
		return entity.Name, nil
	}
	return "", fmt.Errorf("Unsupported entity type %v", entity_type)
}

var entityParentMap = map[string]string{
	monitoring.SLU: models.CLUSTER,
}

func getParentName(queriedEntityType string, parentId uuid.UUID) (string, error) {
	switch queriedEntityType {
	case monitoring.SLU:
		parent, parentFetchErr := GetCluster(&parentId)
		if parentFetchErr != nil {
			return "", fmt.Errorf("%v not a valid id of %v.Error %v", parentId, models.CLUSTER, parentFetchErr)
		}
		return parent.Name, nil
	}
	return "", nil
}

func (a *App) GET_Utilization(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
	}

	var start_time string
	var end_time string
	var interval string
	vars := mux.Vars(r)

	entity_id_str := vars["entity-id"]
	entity_type := vars["entity-type"]

	params := r.URL.Query()
	resource_name := params.Get("resource")
	duration := params.Get("duration")
	parent_id_str := params.Get("parent_id")

	entity_id, entityIdParseError := uuid.Parse(entity_id_str)
	if entityIdParseError != nil {
		HttpResponse(w, http.StatusBadRequest, entityIdParseError.Error())
		logger.Get().Error("%s-%v", ctxt, entityIdParseError.Error())
		return
	}

	var parent_id *uuid.UUID
	var parentError error
	var parentName string
	if parent_id_str != "" {
		parent_id, parentError = uuid.Parse(parent_id_str)
		if parentError != nil {
			HttpResponse(w, http.StatusBadRequest, parentError.Error())
			logger.Get().Error("%s-%v", ctxt, parentError.Error())
			return
		}

		parentName, parentError = getParentName(entity_type, *parent_id)
		if parentError != nil {
			HttpResponse(w, http.StatusBadRequest, parentError.Error())
			logger.Get().Error("%s-%v", ctxt, parentError.Error())
			return
		}
	}

	entityName, entityNameError := getEntityName(entity_type, *entity_id, parent_id)
	if entityNameError != nil {
		HttpResponse(w, http.StatusBadRequest, entityNameError.Error())
		logger.Get().Error("%s-%v", ctxt, entityNameError.Error())
		return
	}

	if duration != "" {
		if strings.Contains(duration, ",") {
			splt := strings.Split(duration, ",")
			start_time = splt[0]
			end_time = splt[1]
		} else {
			interval = duration
		}
	}

	paramsToQuery := map[string]interface{}{"nodename": entityName, "resource": resource_name, "start_time": start_time, "end_time": end_time, "interval": interval}
	if parentName != "" {
		paramsToQuery["parentName"] = parentName
	}

	res, err := GetMonitoringManager().QueryDB(paramsToQuery)
	if err == nil {
		json.NewEncoder(w).Encode(res)
	} else {
		HttpResponse(w, http.StatusInternalServerError, err.Error())
	}
}

func (a *App) Get_SystemUtilization(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
	}

	var start_time string
	var end_time string
	var interval string

	entityName := models.SYSTEM

	params := r.URL.Query()
	resource_name := params.Get("resource")
	duration := params.Get("duration")

	if duration != "" {
		if strings.Contains(duration, ",") {
			splt := strings.Split(duration, ",")
			start_time = splt[0]
			end_time = splt[1]
		} else {
			interval = duration
		}
	}

	paramsToQuery := map[string]interface{}{
		"nodename":   entityName,
		"resource":   resource_name,
		"start_time": start_time,
		"end_time":   end_time,
		"interval":   interval,
	}

	res, err := GetMonitoringManager().QueryDB(paramsToQuery)
	if err == nil {
		json.NewEncoder(w).Encode(res)
	} else {
		logger.Get().Error("%s - Failed to get %v utilization of system.Err %v", ctxt, err)
		HttpResponse(w, http.StatusInternalServerError, err.Error())
	}
}

//In memory ClusterId to ScheduleId map
var ClusterMonitoringSchedules map[uuid.UUID]uuid.UUID

func InitMonitoringSchedules() {
	if ClusterMonitoringSchedules == nil {
		ClusterMonitoringSchedules = make(map[uuid.UUID]uuid.UUID)
	}
	clusters, err := GetClusters()
	if err != nil {
		logger.Get().Error("Error getting the clusters list: %v", err)
		return
	}
	for _, cluster := range clusters {
		ScheduleCluster(cluster.ClusterId, cluster.MonitoringInterval)
	}
}

var mutex sync.Mutex

func SynchroniseScheduleMaintainers(clusterId uuid.UUID) (schedule.Scheduler, error) {
	mutex.Lock()
	defer mutex.Unlock()
	scheduler, err := schedule.NewScheduler()
	if err != nil {
		return scheduler, err
	}
	ClusterMonitoringSchedules[clusterId] = scheduler.Id
	return scheduler, nil
}

func ScheduleCluster(clusterId uuid.UUID, intervalInSecs int) {
	if intervalInSecs == 0 {
		intervalInSecs = monitoring.DefaultClusterMonitoringInterval
	}
	scheduler, err := SynchroniseScheduleMaintainers(clusterId)
	if err != nil {
		logger.Get().Error(err.Error())
	}
	f := GetApp().MonitorCluster
	// Schedule cluster monitoring
	go scheduler.Schedule(time.Duration(intervalInSecs)*time.Second, f, map[string]interface{}{"clusterId": clusterId})
	// Default run so that the user doesn't need to wait for scheduled interval seconds for correct data to appear.
	GetApp().MonitorCluster(map[string]interface{}{"clusterId": clusterId})
}

func DeleteClusterSchedule(clusterId uuid.UUID) {
	mutex.Lock()
	defer mutex.Unlock()
	schedulerId, ok := ClusterMonitoringSchedules[clusterId]
	if !ok {
		logger.Get().Error("Cluster with id %v not scheduled", clusterId)
		return
	}
	if err := schedule.DeleteScheduler(schedulerId); err != nil {
		logger.Get().Error("Failed to delete schedule for cluster %v.Error %v", clusterId, err)
	}
	delete(ClusterMonitoringSchedules, clusterId)
}

func FetchStatFromGraphite(ctxt string, hostname string, resourceName string, counter *int) (value float64) {
	val, isSuccess := FetchStatFromGraphiteWithErrorIndicate(ctxt, hostname, resourceName)
	if !isSuccess {
		*counter = *counter - 1
	}
	return val
}

func FetchStatFromGraphiteWithErrorIndicate(ctxt string, hostname string, resourceName string) (value float64, isSuccess bool) {
	stat, statFetchError := GetMonitoringManager().GetInstantValue(hostname, resourceName)
	if statFetchError != nil {
		logger.Get().Warning("%s - Error %v", ctxt, statFetchError)
		return 0.0, false
	}
	if math.IsNaN(stat) {
		logger.Get().Warning("%s - %v Stat of node %v is nan", ctxt, resourceName, hostname)
		return 0.0, false
	}
	return stat, true
}

func FetchAggregatedStatsFromGraphite(ctxt string, hostname string, resourceName string, counter *int, exceptionList []string) (value float64) {
	stat, statFetchError, statFetchCompleteFailure := GetMonitoringManager().GetInstantValuesAggregation(hostname, resourceName, exceptionList)
	if statFetchError != nil {
		logger.Get().Warning("%s - Error %v", ctxt, statFetchError)
	}
	if statFetchCompleteFailure {
		*counter = *counter - 1
		return 0.0
	}
	if math.IsNaN(stat) {
		*counter = *counter - 1
		logger.Get().Warning("%s - Error %v", ctxt, statFetchError)
		return 0.0
	}
	return stat
}

func AverageAndUpdateDb(ctxt string, mValue float64, count int, time_stamp_str string, tableName string) float64 {
	if count > 0 {
		mValue = mValue / float64(count)
		UpdateMetricToTimeSeriesDb(ctxt, mValue, time_stamp_str, tableName)
	}
	return mValue
}

func UpdateMetricToTimeSeriesDb(ctxt string, mValue float64, time_stamp_str string, tableName string) {
	hostname := conf.SystemConfig.TimeSeriesDBConfig.Hostname
	port := conf.SystemConfig.TimeSeriesDBConfig.DataPushPort
	if err := GetMonitoringManager().PushToDb(map[string]map[string]string{tableName: {time_stamp_str: strconv.FormatFloat(mValue, 'E', -1, 64)}}, hostname, port); err != nil {
		logger.Get().Warning("%s - Error pushing %v statistics to %s.Err %v", ctxt, mValue, tableName, err)
	}
}

func (a *App) MonitorCluster(params map[string]interface{}) {
	clusterId := params["clusterId"]
	reqId, err := uuid.New()
	if err != nil {
		logger.Get().Error("Error Creating the RequestId. error: %v", err)
		return
	}
	ctxt := fmt.Sprintf("%s:%v", models.ENGINE_NAME, reqId.String())

	id, ok := clusterId.(uuid.UUID)
	if !ok {
		logger.Get().Error("%s - Failed to parse cluster id %v", ctxt, clusterId)
		return
	}

	go a.RouteProviderBasedMonitoring(ctxt, id)

	cluster, clusterFetchError := GetCluster(&id)
	if clusterFetchError != nil {
		logger.Get().Error("%s - Unable to get cluster with id %v.Error %v", ctxt, id, clusterFetchError)
		return
	}

	nodes, nodesFetchError := getClusterNodesById(&id)
	if nodesFetchError != nil {
		logger.Get().Error("%s - Failed to fetch nodes in cluster %v. Err %v", ctxt, id, nodesFetchError.Error())
		return
	}

	var cluster_memory_used float64
	var cluster_memory_total float64
	var net_memory_usage_percentage float64

	var disk_reads float64
	disk_reads_count := len(nodes)
	var disk_writes float64
	disk_writes_count := len(nodes)

	var latency float64
	latency_count := len(nodes)
	var cluster_cpu_user float64

	var cluster_interface_rx float64
	cluster_interface_rx_count := len(nodes)
	var cluster_interface_tx float64
	cluster_interface_tx_count := len(nodes)
	var resourceNameError error

	for _, node := range nodes {
		/*
			Calculate Memory Used
		*/
		memoryUtilization := node.Utilizations["memoryusage"]
		cluster_memory_used = cluster_memory_used + float64(memoryUtilization.Used)

		/*
			Calculate Free Memory
		*/
		cluster_memory_total = cluster_memory_total + float64(memoryUtilization.Total)

		/*
			Calculate cpu user utilization
		*/
		cpuUser := node.Utilizations["cpuusage"].PercentUsed
		cluster_cpu_user = cluster_cpu_user + cpuUser

		/*
			Calculate Latency
		*/
		resource_name, resource_name_error := GetMonitoringManager().GetResourceName(map[string]interface{}{"serverName": strings.Replace(curr_hostname, ".", "_", -1), "resource_name": monitoring.NETWORK_LATENCY})
		if resource_name_error == nil {
			latency = latency + FetchStatFromGraphite(ctxt, node.Hostname, resource_name, &latency_count)
		} else {
			logger.Get().Warning("%s - Failed to fetch latency statistics from %v.Error %v", ctxt, node.Hostname, resource_name_error)
		}

		// Aggregate disk read
		resourcePrefix := monitoring.AGGREGATION + monitoring.DISK
		resource_name, resourceNameError = GetMonitoringManager().GetResourceName(map[string]interface{}{"resource_name": resourcePrefix + monitoring.READ})
		if resourceNameError != nil {
			logger.Get().Warning("%s - Failed to fetch resource name of %v for %v from cluster%v.Err %v", ctxt, resource_name, node.Hostname, clusterId, resourceNameError)
			disk_reads_count = disk_reads_count - 1
		} else {
			disk_reads = disk_reads + FetchAggregatedStatsFromGraphite(ctxt, node.Hostname, resource_name, &disk_reads_count, []string{})
		}

		// Aggregate disk write
		resource_name, resourceNameError = GetMonitoringManager().GetResourceName(map[string]interface{}{"resource_name": resourcePrefix + monitoring.WRITE})
		if resourceNameError != nil {
			logger.Get().Warning("%s - Failed to fetch resource name of %v for %v from cluster%v.Err %v", ctxt, resource_name, node.Hostname, clusterId, resourceNameError)
			disk_writes_count = disk_writes_count - 1
		} else {
			disk_writes = disk_writes + FetchAggregatedStatsFromGraphite(ctxt, node.Hostname, resource_name, &disk_writes_count, []string{})
		}

		// Aggregate interface rx
		resourcePrefix = monitoring.AGGREGATION + monitoring.INTERFACE + monitoring.OCTETS
		resource_name, resourceNameError = GetMonitoringManager().GetResourceName(map[string]interface{}{"resource_name": resourcePrefix + monitoring.RX})
		if resourceNameError != nil {
			logger.Get().Warning("%s - Failed to fetch resource name of %v for %v from cluster%v.Err %v", ctxt, resourcePrefix+monitoring.RX, node.Hostname, clusterId, resourceNameError)
			cluster_interface_rx_count = cluster_interface_rx_count - 1
		} else {
			cluster_interface_rx = cluster_interface_rx + FetchAggregatedStatsFromGraphite(ctxt, node.Hostname, resource_name, &cluster_interface_rx_count, []string{monitoring.LOOP_BACK_INTERFACE})
		}

		// Aggregate interface tx
		resource_name, resourceNameError = GetMonitoringManager().GetResourceName(map[string]interface{}{"resource_name": resourcePrefix + monitoring.TX})
		if resourceNameError != nil {
			logger.Get().Warning("%s - Failed to fetch resource name of %v for %v from cluster%v.Err %v", ctxt, resource_name, node.Hostname, clusterId, resourceNameError)
			cluster_interface_tx_count = cluster_interface_tx_count - 1
		} else {
			cluster_interface_tx = cluster_interface_tx + FetchAggregatedStatsFromGraphite(ctxt, node.Hostname, resource_name, &cluster_interface_tx_count, []string{monitoring.LOOP_BACK_INTERFACE})
		}
	}

	time_stamp_str := strconv.FormatInt(time.Now().Unix(), 10)

	table_name := conf.SystemConfig.TimeSeriesDBConfig.CollectionName + "." + strings.Replace(cluster.Name, ".", "_", -1) + "."

	disk_reads = AverageAndUpdateDb(ctxt, disk_reads, disk_reads_count, time_stamp_str, table_name+monitoring.DISK+"-"+monitoring.READ)
	disk_writes = AverageAndUpdateDb(ctxt, disk_writes, disk_writes_count, time_stamp_str, table_name+monitoring.DISK+"-"+monitoring.WRITE)
	if disk_reads_count > 0 && disk_writes_count > 0 {
		UpdateMetricToTimeSeriesDb(ctxt, disk_reads+disk_writes, time_stamp_str, fmt.Sprintf("%s%s-%s_%s", table_name, monitoring.DISK, monitoring.READ, monitoring.WRITE))
	}

	if cluster_memory_total > 0 {
		UpdateMetricToTimeSeriesDb(ctxt, cluster_memory_used, time_stamp_str, table_name+monitoring.MEMORY+"-"+monitoring.USED_SPACE)
		UpdateMetricToTimeSeriesDb(ctxt, cluster_memory_total, time_stamp_str, table_name+monitoring.MEMORY+"-"+monitoring.TOTAL_SPACE)
	}
	AverageAndUpdateDb(ctxt, cluster_cpu_user, len(nodes), time_stamp_str, fmt.Sprintf("%s%s-%s", table_name, monitoring.CPU_USER, monitoring.CPU_SYSTEM))
	AverageAndUpdateDb(ctxt, latency, latency_count, time_stamp_str, table_name+monitoring.NETWORK_LATENCY)

	var cluster_memory_percentage float64
	if cluster_memory_total != 0 {
		cluster_memory_percentage = float64(cluster_memory_used*100) / float64(cluster_memory_total)
	}
	clusterUtilizations := map[string]interface{}{
		"memoryusage": models.Utilization{
			Used:        int64(cluster_memory_used),
			Total:       int64(cluster_memory_total),
			PercentUsed: cluster_memory_percentage,
			UpdatedAt:   time.Now().String(),
		},
		"cpupercentageusage": float64(cluster_cpu_user) / float64(len(nodes)),
	}

	sessionCopy := db.GetDatastore().Copy()
	defer sessionCopy.Close()

	coll := sessionCopy.DB(conf.SystemConfig.DBConfig.Database).C(models.COLL_NAME_STORAGE_CLUSTERS)
	if coll.Update(
		bson.M{"clusterid": cluster.ClusterId},
		bson.M{"$set": bson.M{"utilizations": clusterUtilizations}}); err != nil {
		logger.Get().Warning("%s - Failed to update memory and cpu utilizations of cluster %v to db.Error %v", ctxt, cluster.Name, err)
	}

	cluster_interface_rx = AverageAndUpdateDb(ctxt, cluster_interface_rx, cluster_interface_rx_count, time_stamp_str, table_name+monitoring.INTERFACE+"-"+monitoring.RX)
	cluster_interface_tx = AverageAndUpdateDb(ctxt, cluster_interface_tx, cluster_interface_tx_count, time_stamp_str, table_name+monitoring.INTERFACE+"-"+monitoring.TX)
	if cluster_interface_rx_count > 0 && cluster_interface_tx_count > 0 {
		UpdateMetricToTimeSeriesDb(ctxt, cluster_interface_rx+cluster_interface_tx, time_stamp_str, fmt.Sprintf("%s%s-%s_%s", table_name, monitoring.INTERFACE, monitoring.RX, monitoring.TX))
	}

	hostname := conf.SystemConfig.TimeSeriesDBConfig.Hostname
	port := conf.SystemConfig.TimeSeriesDBConfig.DataPushPort
	if cluster_memory_total != 0.0 {
		net_memory_usage_percentage = (cluster_memory_used * 100) / cluster_memory_total
		memory_percent_table := table_name + monitoring.MEMORY + "-" + monitoring.USAGE_PERCENT
		if err = GetMonitoringManager().PushToDb(map[string]map[string]string{memory_percent_table: {time_stamp_str: strconv.FormatFloat(net_memory_usage_percentage, 'E', -1, 64)}}, hostname, port); err != nil {
			logger.Get().Warning("%s - Error pushing cluster memory utilization.Err %v", ctxt, err)
		}
	}

	return
}

func ParseStatFromCollectd(mValue string) (float64, error) {
	value, valueErr := strconv.ParseFloat(mValue, 64)
	if valueErr != nil {
		return 0.0, fmt.Errorf("Failed to parse stat %s.Error %v", mValue, valueErr)
	}
	return value, nil
}

func (a *App) POST_AddMonitoringPlugin(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
	}

	vars := mux.Vars(r)
	cluster_id, cluster_id_parse_error := uuid.Parse(vars["cluster-id"])
	if cluster_id_parse_error != nil {
		logger.Get().Error("%s-Error parsing the cluster id: %s. error: %v", vars["cluster-id"], ctxt, cluster_id_parse_error)
		HttpResponse(w, http.StatusInternalServerError, cluster_id_parse_error.Error())
		return
	}
	var request monitoring.Plugin
	body, err := ioutil.ReadAll(io.LimitReader(r.Body, models.REQUEST_SIZE_LIMIT))
	if err != nil {
		logger.Get().Error("%s-Error parsing the request. error: %v", ctxt, err)
		HttpResponse(w, http.StatusBadRequest, "Unable to parse the request")
		return
	}
	if err := json.Unmarshal(body, &request); err != nil {
		logger.Get().Error("%s-Unable to unmarshal request. error: %v", ctxt, err)
		HttpResponse(w, http.StatusBadRequest, "Unable to unmarshal request")
		return
	}
	cluster, clusterFetchErr := GetCluster(cluster_id)
	if clusterFetchErr != nil {
		logger.Get().Error("%s-Failed to add monitoring configuration for cluster: %v.Error %v", ctxt, *cluster_id, clusterFetchErr)
		if clusterFetchErr == mgo.ErrNotFound {
			HttpResponse(w, http.StatusNotFound, fmt.Sprintf("Failed to add monitoring configuration for cluster: %v.Error %v", *cluster_id, clusterFetchErr))

		} else {
			HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Failed to add monitoring configuration for cluster: %v.Error %v", *cluster_id, clusterFetchErr))
		}
		return
	}

	if cluster.State == models.CLUSTER_STATE_UNMANAGED {
		logger.Get().Error("%s-Cluster: %v is in un-managed state", ctxt, *cluster_id)
		HttpResponse(w, http.StatusMethodNotAllowed, "Cluster is in un-managed state")
		return
	}

	for _, plugin := range cluster.Monitoring.Plugins {
		if plugin.Name == request.Name {
			logger.Get().Error("%s-Plugin %v already exists on cluster %v", ctxt, request.Name, cluster.Name)
			HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Plugin %v already exists in cluster %v", request.Name, cluster.Name))
			return
		}
	}
	nodes, err := getClusterNodesById(cluster_id)
	if err != nil {
		logger.Get().Error(fmt.Sprintf("%s-Failed to get nodes for locking for cluster: %v.Error %v", ctxt, *cluster_id, err))
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Failed to get nodes for locking for cluster: %v.Error %v", *cluster_id, err))
		return
	}
	var cluster_node_names []string
	var down_nodes []string
	for _, node := range nodes {
		cluster_node_names = append(cluster_node_names, node.Hostname)
		if node.Status == models.NODE_STATUS_ERROR {
			down_nodes = append(down_nodes, node.Hostname)
		}
	}
	if len(down_nodes) == len(cluster_node_names) {
		logger.Get().Error("%s-All nodes in cluster %v are down", ctxt, cluster.Name)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("All nodes in cluster %v are down", cluster.Name))
		return
	}
	asyncTask := func(t *task.Task) {
		for {
			select {
			case <-t.StopCh:
				return
			default:
				var monState models.MonitoringState
				var nodesWithStaleMonitoringConfig = util.NewSetWithType(reflect.TypeOf(""))
				nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(down_nodes))
				nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(cluster.Monitoring.StaleNodes))
				monState.StaleNodes, _ = util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())

				t.UpdateStatus("Started task to add the monitoring plugin : %v", t.ID)
				appLock, err := LockNodes(ctxt, nodes, "POST_AddMonitoringPlugin")
				if err != nil {
					util.FailTask("Failed to acquire lock", fmt.Errorf("%s-%v", ctxt, err), t)
					return
				}
				defer a.GetLockManager().ReleaseLock(ctxt, *appLock)
				addNodeWiseErrors, addError := GetCoreNodeManager().AddMonitoringPlugin(cluster_node_names, "", request, ctxt)
				if len(addNodeWiseErrors) != 0 {
					//The only error that GetMapKeys is if it doesn't get a map as param as it takes interface for the flexibility of handling any kind of input map
					// It is guranteed that AddMonitoringPlugin specifically returns a map else it would fail much before coming here.
					nodesInErrorValues, _ := util.GetMapKeys(addNodeWiseErrors)
					nodesInError := util.Stringify(nodesInErrorValues)
					if addError != nil {
						logger.Get().Error("%s-%v", ctxt, addError.Error())
					}
					nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(nodesInError))
					staleMonitoringNodes, _ := util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())
					monState.StaleNodes = staleMonitoringNodes
					logger.Get().Error("%s-Failed to add monitoring configuration for the cluster %v. Error :%v", ctxt, cluster.Name, addNodeWiseErrors)
					t.UpdateStatus("Failed to add monitoring configuration for the cluster %v. Error :%v", cluster.Name, addNodeWiseErrors)
				}
				updatedPlugins := append(cluster.Monitoring.Plugins, request)
				t.UpdateStatus("Updating the plugins to db")
				monState.Plugins = updatedPlugins
				if dbError := updatePluginsInDb(bson.M{"clusterid": cluster_id}, monState); dbError != nil {
					util.FailTask(fmt.Sprintf("Failed to add monitoring configuration for cluster: %v", *cluster_id), fmt.Errorf("%s-%v", ctxt, dbError), t)
					return
				}
				if len(monState.StaleNodes) == len(cluster_node_names) {
					util.FailTask(fmt.Sprintf("Failed to add monitoring configuration for cluster: %v", *cluster_id), fmt.Errorf("%s-%v", ctxt, addNodeWiseErrors), t)
					return
				}
				t.UpdateStatus("Success")
				t.Done(models.TASK_STATUS_SUCCESS)
				return
			}
		}
	}
	if taskId, err := a.GetTaskManager().Run(
		models.ENGINE_NAME,
		fmt.Sprintf("Create Cluster: %s", request.Name),
		asyncTask,
		nil,
		nil,
		nil); err != nil {
		logger.Get().Error("%s-Unable to create task for adding monitoring plugin for cluster: %v. error: %v", ctxt, *cluster_id, err)
		HttpResponse(w, http.StatusInternalServerError, "Task creation failed for add monitoring plugin")
		return
	} else {
		logger.Get().Debug("%s-Task Created: %v for adding moniroring plugin for cluster: %v", ctxt, taskId, *cluster_id)
		bytes, _ := json.Marshal(models.AsyncResponse{TaskId: taskId})
		w.WriteHeader(http.StatusAccepted)
		w.Write(bytes)
	}
}

func updatePluginsInDb(parameter bson.M, monitoringState models.MonitoringState) (err error) {
	sessionCopy := db.GetDatastore().Copy()
	defer sessionCopy.Close()
	coll := sessionCopy.DB(conf.SystemConfig.DBConfig.Database).C(models.COLL_NAME_STORAGE_CLUSTERS)
	dbUpdateError := coll.Update(parameter, bson.M{"$set": bson.M{"monitoring": monitoringState}})
	return dbUpdateError
}

func (a *App) PUT_Thresholds(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
	}

	var request []monitoring.Plugin = make([]monitoring.Plugin, 0)
	vars := mux.Vars(r)
	cluster_id := vars["cluster-id"]

	// Unmarshal the request body
	body, err := ioutil.ReadAll(io.LimitReader(r.Body, models.REQUEST_SIZE_LIMIT))
	if err != nil {
		logger.Get().Error("%s-Error parsing the threshold update request for the cluster %v. error: %v", ctxt, cluster_id, err)
		HttpResponse(w, http.StatusBadRequest, "Unable to parse the request")
		return
	}
	if err := json.Unmarshal(body, &request); err != nil {
		logger.Get().Error("%s-Unable to unmarshall the threshold update request of cluster %v. error: %v", ctxt, cluster_id, err)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Unable to unmarshal threshold update request on cluster %v. error: %v", cluster_id, err))
		return
	}
	cluster_id_uuid, cluster_id_parse_error := uuid.Parse(cluster_id)
	if cluster_id_parse_error != nil {
		logger.Get().Error("%s-Failed to parse cluster id %v. error: %v", ctxt, cluster_id, cluster_id_parse_error)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Failed to parse cluster id %v. error: %v", cluster_id, cluster_id_parse_error))
		return
	}
	cluster, clusterFetchErr := GetCluster(cluster_id_uuid)
	if clusterFetchErr != nil {
		logger.Get().Error("%s-Failed to get cluster with id %v. error: %v", ctxt, cluster_id, clusterFetchErr)
		if clusterFetchErr == mgo.ErrNotFound {
			HttpResponse(w, http.StatusNotFound, fmt.Sprintf("Failed to get cluster with id %v. error: %v", cluster_id, clusterFetchErr))
		} else {
			HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Failed to get cluster with id %v. error: %v", cluster_id, clusterFetchErr))
		}
		return
	}

	if cluster.State == models.CLUSTER_STATE_UNMANAGED {
		logger.Get().Error("%s-Cluster: %v is in un-managed state", ctxt, cluster_id_uuid)
		HttpResponse(w, http.StatusMethodNotAllowed, "Cluster is in un-managed state")
		return
	}

	nodes, err := getClusterNodesById(cluster_id_uuid)
	if err != nil {
		logger.Get().Error("%s-Failed to get nodes of cluster id %v. error: %v", ctxt, cluster_id, err)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Failed to get nodes of cluster id %v. error: %v", cluster_id, err))
		return
	}
	var cluster_node_names []string
	var down_nodes []string
	for _, node := range nodes {
		cluster_node_names = append(cluster_node_names, node.Hostname)
		if node.Status == models.NODE_STATUS_ERROR {
			down_nodes = append(down_nodes, node.Hostname)
		}
	}
	if len(down_nodes) == len(cluster_node_names) {
		logger.Get().Error("%s-All nodes in cluster %v are down", ctxt, cluster.Name)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("All nodes in cluster %v are down", cluster.Name))
		return
	}
	if len(request) == 0 {
		logger.Get().Error("%s-No thresholds passed for configuration in cluster %v", ctxt, cluster.Name)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("No thresholds passed for configuration in cluster %v", cluster.Name))
		return
	}
	asyncTask := func(t *task.Task) {
		for {
			select {
			case <-t.StopCh:
				return
			default:
				var monState models.MonitoringState
				var nodesWithStaleMonitoringConfig = util.NewSetWithType(reflect.TypeOf(""))
				nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(down_nodes))
				nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(cluster.Monitoring.StaleNodes))
				monState.StaleNodes, _ = util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())
				t.UpdateStatus("Started task to update monitoring plugins configuration : %v", t.ID)
				var updatedPlugins []monitoring.Plugin
				var pluginUpdateError error
				appLock, err := LockNodes(ctxt, nodes, "PUT_Thresholds")
				if err != nil {
					util.FailTask("Failed to acquire lock", fmt.Errorf("%s-%v", ctxt, err), t)
					return
				}
				defer a.GetLockManager().ReleaseLock(ctxt, *appLock)
				if updatedPlugins, pluginUpdateError = monitoring.UpdatePluginsConfigs(cluster.Monitoring.Plugins, request); pluginUpdateError != nil {
					util.FailTask(fmt.Sprintf("Failed to update thresholds for cluster: %v", *cluster_id_uuid), fmt.Errorf("%s-%v", ctxt, pluginUpdateError), t)
					return
				}
				updateConfigurationErrors, updateErr := GetCoreNodeManager().UpdateMonitoringConfiguration(
					cluster_node_names,
					request,
					ctxt)
				if len(updateConfigurationErrors) != 0 {
					updateConfigurationErrorValues, _ := util.GetMapKeys(updateConfigurationErrors)
					updateFailedNodes := util.Stringify(updateConfigurationErrorValues)
					if updateErr != nil {
						logger.Get().Error("%s-Failed to update thresholds for cluster: %v.Error: %v", ctxt, *cluster_id_uuid, updateErr.Error())
					}
					nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(updateFailedNodes))
					staleMonitoringNodes, _ := util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())
					monState.StaleNodes = staleMonitoringNodes
					logger.Get().Error("%s-Failed to update monitoring configuration on %v of cluster %v", ctxt, updateFailedNodes, cluster.Name)
					t.UpdateStatus("Failed to update monitoring configuration on %v of cluster %v", updateFailedNodes, cluster.Name)
				}
				monState.Plugins = updatedPlugins
				t.UpdateStatus("Updating new configuration to db")
				if dbError := updatePluginsInDb(bson.M{"clusterid": cluster_id_uuid}, monState); dbError != nil {
					util.FailTask(fmt.Sprintf("Failed to update thresholds for cluster: %v", *cluster_id_uuid), fmt.Errorf("%s-%v", ctxt, dbError), t)
					return
				}
				if len(monState.StaleNodes) == len(cluster_node_names) {
					util.FailTask(fmt.Sprintf("Failed to update thresholds for cluster: %v", *cluster_id_uuid), fmt.Errorf("%s-%v", ctxt, updateConfigurationErrors), t)
					return
				}
				t.UpdateStatus("Success")
				t.Done(models.TASK_STATUS_SUCCESS)
			}
			return
		}
	}
	if taskId, err := a.GetTaskManager().Run(
		models.ENGINE_NAME,
		"Update monitoring plugins configuration",
		asyncTask,
		nil,
		nil,
		nil); err != nil {
		logger.Get().Error("%s-Unable to create task for update monitoring plugin configuration for cluster: %v. error: %v", ctxt, *cluster_id_uuid, err)
		HttpResponse(w, http.StatusInternalServerError, "Task creation failed for update monitoring plugin configuration")
		return
	} else {
		logger.Get().Debug("%s-Task Created: %v for updating monitoring plugin thresholds for cluster: %v", ctxt, taskId, cluster_id_uuid)
		bytes, _ := json.Marshal(models.AsyncResponse{TaskId: taskId})
		w.WriteHeader(http.StatusAccepted)
		w.Write(bytes)
	}
}

func (a *App) POST_froceUpdateMonitoringConfiguration(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
	}

	vars := mux.Vars(r)
	var cluster models.Cluster
	cluster_id, err := uuid.Parse(vars["cluster-id"])
	if err != nil {
		logger.Get().Error("%s-%v", ctxt, err.Error())
		HttpResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if cluster, err = GetCluster(cluster_id); err != nil {
		logger.Get().Error("%s-%v", ctxt, err.Error())
		if err == mgo.ErrNotFound {
			HttpResponse(w, http.StatusNotFound, err.Error())
		} else {
			HttpResponse(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	if cluster.State == models.CLUSTER_STATE_UNMANAGED {
		logger.Get().Error("%s-Cluster: %v is in un-managed state", ctxt, *cluster_id)
		HttpResponse(w, http.StatusMethodNotAllowed, "Cluster is in un-managed state")
		return
	}

	if len(cluster.Monitoring.StaleNodes) == 0 {
		logger.Get().Error("%s-All nodes in the cluster %v have fresh monitoring configurations", ctxt, cluster.Name)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("All nodes in the cluster %v have fresh monitoring configurations", cluster.Name))
		return
	}
	asyncTask := func(t *task.Task) {
		if pluginUpdateErr := forceUpdatePlugins(cluster, cluster.Monitoring.StaleNodes, ctxt); pluginUpdateErr != nil {
			util.FailTask(fmt.Sprintf("Failed to restore monitoring configuration on cluster %v", cluster.Name), fmt.Errorf("%s-%v", ctxt, pluginUpdateErr), t)
			return
		}
		t.UpdateStatus("Success")
		t.Done(models.TASK_STATUS_SUCCESS)
	}
	if taskId, err := a.GetTaskManager().Run(
		models.ENGINE_NAME,
		fmt.Sprintf("Enforce monitoring plugins update"),
		asyncTask,
		nil,
		nil,
		nil); err != nil {
		logger.Get().Error("%s-Unable to create task for enforcing monitoring plugin update. error: %v", ctxt, err)
		HttpResponse(w, http.StatusInternalServerError, "Task creation failed for enforcing monitoring plugin update")
		return
	} else {
		logger.Get().Debug("%s-Task Created: ", ctxt, taskId.String())
		bytes, _ := json.Marshal(models.AsyncResponse{TaskId: taskId})
		w.WriteHeader(http.StatusAccepted)
		w.Write(bytes)
	}
}

func forceUpdatePlugins(cluster models.Cluster, nodes []string, ctxt string) error {
	var monState models.MonitoringState
	var nodesWithStaleMonitoringConfig = util.NewSetWithType(reflect.TypeOf(""))
	nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(cluster.Monitoring.StaleNodes))
	nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(nodes))
	monState.Plugins = cluster.Monitoring.Plugins
	var plugin_names []string
	for _, plugin := range cluster.Monitoring.Plugins {
		if util.StringInSlice(plugin.Name, monitoring.SupportedMonitoringPlugins) {
			plugin_names = append(plugin_names, plugin.Name)
		}
	}
	if forUpdateErrors, forceUpdatePythonError := GetCoreNodeManager().EnforceMonitoring(
		plugin_names,
		nodes,
		"",
		cluster.Monitoring.Plugins,
		ctxt); len(forUpdateErrors) != 0 || forceUpdatePythonError != nil {
		if forceUpdatePythonError != nil {
			return fmt.Errorf("Failed to update monitoring configuration on nodes : %v of cluster: %v.Error: %v", nodes, cluster.Name, forceUpdatePythonError)
		}
		nodesInErrorValues, _ := util.GetMapKeys(forUpdateErrors)
		nodesInError := util.Stringify(nodesInErrorValues)

		nodesInSuccess := util.StringSetDiff(nodes, nodesInError)
		for _, nodeInSuccess := range util.GenerifyStringArr(nodesInSuccess) {
			nodesWithStaleMonitoringConfig.Remove(nodeInSuccess)
		}
		monState.StaleNodes, err = util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())

		if dbError := updatePluginsInDb(bson.M{"clusterid": cluster.ClusterId}, monState); dbError != nil {
			if len(monState.StaleNodes) != 0 {
				return fmt.Errorf("Updating monitoring configuration failed on %v and failed to persist the failure to db.Error: %v", monState.StaleNodes, dbError)
			}
			return fmt.Errorf("Failed to update monitoring configuration to db.Error %v", dbError)
		}
		return nil
	}
	for _, nodeInSuccess := range nodes {
		nodesWithStaleMonitoringConfig.Remove(nodeInSuccess)
	}
	staleNodes, _ := util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())
	monState.StaleNodes = staleNodes
	if dbError := updatePluginsInDb(bson.M{"clusterid": cluster.ClusterId}, monState); dbError != nil {
		return fmt.Errorf("Failed to update monitoring configuration to db for the cluster %v. Error %v", cluster.Name, dbError)
	}
	return nil
}

func monitoringPluginActivationDeactivations(ctxt string, enable bool, plugin_name string, cluster_id *uuid.UUID, w http.ResponseWriter, a *App) {
	var action string
	cluster, err := GetCluster(cluster_id)
	if err != nil {
		logger.Get().Error("%s-Error getting cluster with id: %v. error: %v", ctxt, *cluster_id, err)
		if err == mgo.ErrNotFound {
			HttpResponse(w, http.StatusNotFound, err.Error())
		} else {
			HttpResponse(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	if cluster.State == models.CLUSTER_STATE_UNMANAGED {
		logger.Get().Error("%s-Cluster: %v is in un-managed state", ctxt, *cluster_id)
		HttpResponse(w, http.StatusMethodNotAllowed, "Cluster is in un-managed state")
		return
	}

	plugin_index := -1
	for index, plugin := range cluster.Monitoring.Plugins {
		if plugin.Name == plugin_name {
			if plugin.Enable != enable {
				plugin_index = index
				break
			}
		}
	}
	if enable {
		action = "enable"
	} else {
		action = "disable"
	}
	if plugin_index == -1 {
		logger.Get().Error("%s-Plugin is either already %vd or not configured on cluster: %s", ctxt, action, cluster.Name)
		HttpResponse(w, http.StatusInternalServerError, fmt.Sprintf("Plugin is either already %vd or not configured", action))
		return
	}
	if !monitoring.Contains(plugin_name, monitoring.SupportedMonitoringPlugins) {
		logger.Get().Error("%s-Unsupported plugin: %s for cluster: %s", ctxt, plugin_name, cluster.Name)
		HttpResponse(w, http.StatusInternalServerError, "Unsupported plugin")
	}
	nodes, nodesFetchError := getClusterNodesById(cluster_id)
	if nodesFetchError != nil {
		logger.Get().Error("%s-Unbale to get nodes for cluster: %v. error: %v", ctxt, cluster.Name, nodesFetchError)
		HttpResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	var cluster_node_names []string
	var down_nodes []string
	for _, node := range nodes {
		cluster_node_names = append(cluster_node_names, node.Hostname)
		if node.Status == models.NODE_STATUS_ERROR {
			down_nodes = append(down_nodes, node.Hostname)
		}
	}
	if len(down_nodes) == len(cluster_node_names) {
		logger.Get().Error("%s-All nodes in cluster %v are down", ctxt, cluster.Name)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("All nodes in cluster %v are down", cluster.Name))
		return
	}
	asyncTask := func(t *task.Task) {
		for {
			select {
			case <-t.StopCh:
				return
			default:
				var monState models.MonitoringState
				var nodesWithStaleMonitoringConfig = util.NewSetWithType(reflect.TypeOf(""))
				nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(down_nodes))
				nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(cluster.Monitoring.StaleNodes))
				monState.StaleNodes, _ = util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())
				t.UpdateStatus("Started task to %v monitoring plugin : %v", action, t.ID)
				var actionNodeWiseFailure map[string]string
				var actionErr error
				appLock, err := LockNodes(ctxt, nodes, "monitoringPluginActivationDeactivations")
				if err != nil {
					util.FailTask("Failed to acquire lock", fmt.Errorf("%s-%v", ctxt, err), t)
					return
				}
				defer a.GetLockManager().ReleaseLock(ctxt, *appLock)
				if enable {
					actionNodeWiseFailure, actionErr = GetCoreNodeManager().EnableMonitoringPlugin(
						cluster_node_names,
						plugin_name,
						ctxt)
				} else {
					actionNodeWiseFailure, actionErr = GetCoreNodeManager().DisableMonitoringPlugin(
						cluster_node_names,
						plugin_name,
						ctxt)
				}
				if len(actionNodeWiseFailure) != 0 {
					if actionErr != nil {
						logger.Get().Error("%s-%v", ctxt, actionErr.Error())
					}
					//The only error that GetMapKeys is if it doesn't get a map as param as it takes interface for the flexibility of handling any kind of input map
					// It is guranteed that both EnableMonitoringPlugin and DisableMonitoringPlugin specifically returns a map else it would fail much before coming here.
					nodesInError, _ := util.GetMapKeys(actionNodeWiseFailure)
					updateFailedNodes := util.Stringify(nodesInError)
					nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(updateFailedNodes))
					staleMonitoringNodes, _ := util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())
					monState.StaleNodes = staleMonitoringNodes
					logger.Get().Error("%s-Failed to %v plugin on %v of cluster %v", ctxt, action, updateFailedNodes, cluster.Name)
					t.UpdateStatus("Failed to %v plugin on %v of cluster %v", action, updateFailedNodes, cluster.Name)
				}
				index := monitoring.GetPluginIndex(plugin_name, cluster.Monitoring.Plugins)
				cluster.Monitoring.Plugins[index].Enable = enable
				t.UpdateStatus("Updating changes to db")
				monState.Plugins = cluster.Monitoring.Plugins
				if dbError := updatePluginsInDb(bson.M{"clusterid": cluster_id}, monState); dbError != nil {
					util.FailTask(fmt.Sprintf("Failed to %s plugin %s on cluster: %s", action, plugin_name, cluster.Name), fmt.Errorf("%s-%v", ctxt, dbError), t)
					return
				}
				if len(monState.StaleNodes) == len(cluster_node_names) {
					util.FailTask(fmt.Sprintf("Failed to %s plugin %s on cluster: %s", action, plugin_name, cluster.Name), fmt.Errorf("%s0-%v plugin %s failed on cluster %v", ctxt, action, plugin_name, cluster.Name), t)
					return
				}
				t.UpdateStatus("Success")
				t.Done(models.TASK_STATUS_SUCCESS)
				return
			}
		}
	}
	if taskId, err := a.GetTaskManager().Run(
		models.ENGINE_NAME,
		fmt.Sprintf("%s monitoring plugin: %s", action, plugin_name),
		asyncTask,
		nil,
		nil,
		nil); err != nil {
		logger.Get().Error("%s-Unable to create task for %s monitoring plugin on cluster: %s. error: %v", ctxt, action, cluster.Name, err)
		HttpResponse(w, http.StatusInternalServerError, "Task creation failed for"+action+"monitoring plugin")
		return
	} else {
		logger.Get().Debug("%s-Task Created: %v for %s monitoring plugin on cluster: %s", ctxt, taskId, action, cluster.Name)
		bytes, _ := json.Marshal(models.AsyncResponse{TaskId: taskId})
		w.WriteHeader(http.StatusAccepted)
		w.Write(bytes)
	}
}

func (a *App) POST_MonitoringPluginEnable(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
	}

	vars := mux.Vars(r)

	cluster_id, cluster_id_parse_error := uuid.Parse(vars["cluster-id"])
	if cluster_id_parse_error != nil {
		logger.Get().Error("%s-Error parsing request. error: %v", ctxt, cluster_id_parse_error)
		HttpResponse(w, http.StatusBadRequest, cluster_id_parse_error.Error())
		return
	}
	plugin_name := vars["plugin-name"]
	monitoringPluginActivationDeactivations(ctxt, true, plugin_name, cluster_id, w, a)
}

func (a *App) POST_MonitoringPluginDisable(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
	}

	vars := mux.Vars(r)

	cluster_id, cluster_id_parse_error := uuid.Parse(vars["cluster-id"])
	if cluster_id_parse_error != nil {
		logger.Get().Error("%s-Error parsing the request. error: %v", ctxt, cluster_id_parse_error)
		HttpResponse(w, http.StatusBadRequest, cluster_id_parse_error.Error())
		return
	}
	plugin_name := vars["plugin-name"]
	monitoringPluginActivationDeactivations(ctxt, false, plugin_name, cluster_id, w, a)
}

func (a *App) REMOVE_MonitoringPlugin(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
	}

	vars := mux.Vars(r)
	cluster_id := vars["cluster-id"]
	uuid, err := uuid.Parse(cluster_id)
	if err != nil {
		logger.Get().Error("%s-Error parsing the request cluster id: %s. error: %v", ctxt, cluster_id, err)
		HttpResponse(w, http.StatusMethodNotAllowed, err.Error())
		return
	}
	plugin_name := vars["plugin-name"]
	cluster, clusterFetchErr := GetCluster(uuid)
	if clusterFetchErr != nil {
		logger.Get().Error("%s-Failed to remove plugin %s for cluster: %v.Error %v", ctxt, *uuid, plugin_name, clusterFetchErr)
		HttpResponse(w, http.StatusMethodNotAllowed, fmt.Sprintf("Failed to remove plugin %s for cluster: %v.Error %v", *uuid, plugin_name, clusterFetchErr))
		return
	}

	if cluster.State == models.CLUSTER_STATE_UNMANAGED {
		logger.Get().Error("%s-Cluster: %v is in un-managed state", ctxt, *uuid)
		HttpResponse(w, http.StatusMethodNotAllowed, "Cluster is in un-managed state")
		return
	}

	nodes, nodesFetchError := getClusterNodesById(uuid)
	if nodesFetchError != nil {
		logger.Get().Error("%s-Unbale to get nodes for cluster: %v. error: %v", ctxt, *uuid, nodesFetchError)
		HttpResponse(w, http.StatusInternalServerError, nodesFetchError.Error())
	}
	var cluster_node_names []string
	var down_nodes []string
	for _, node := range nodes {
		cluster_node_names = append(cluster_node_names, node.Hostname)
		if node.Status == models.NODE_STATUS_ERROR {
			down_nodes = append(down_nodes, node.Hostname)
		}
	}
	if len(down_nodes) == len(cluster_node_names) {
		logger.Get().Error("%s-All nodes in cluster %v are down", ctxt, cluster.Name)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("All nodes in cluster %v are down", cluster.Name))
		return
	}
	if monitoring.GetPluginIndex(plugin_name, cluster.Monitoring.Plugins) == -1 {
		logger.Get().Error("%s-Plugin %v already deleted on cluster %v", ctxt, plugin_name, cluster.Name)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Plugin %v already deleted on cluster %v", plugin_name, cluster.Name))
		return
	}
	asyncTask := func(t *task.Task) {
		for {
			select {
			case <-t.StopCh:
				return
			default:
				var monState models.MonitoringState
				var nodesWithStaleMonitoringConfig = util.NewSetWithType(reflect.TypeOf(""))
				nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(cluster.Monitoring.StaleNodes))
				nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(down_nodes))
				monState.StaleNodes, _ = util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())
				t.UpdateStatus("Task created to remove monitoring plugin %v", plugin_name)
				appLock, err := LockNodes(ctxt, nodes, "REMOVE_MonitoringPlugin")
				if err != nil {
					util.FailTask("Failed to acquire lock", fmt.Errorf("%s-%v", ctxt, err), t)
					return
				}
				defer a.GetLockManager().ReleaseLock(ctxt, *appLock)
				removeNodeWiseFailure, removeErr := GetCoreNodeManager().RemoveMonitoringPlugin(
					cluster_node_names,
					plugin_name,
					ctxt)
				if len(removeNodeWiseFailure) != 0 || removeErr != nil {
					//The only error that GetMapKeys is if it doesn't get a map as param as it takes interface for the flexibility of handling any kind of input map
					// It is guranteed that RemoveMonitoringPlugin specifically returns a map else it would fail much before coming here.
					nodesInErrorValues, _ := util.GetMapKeys(removeNodeWiseFailure)
					nodesInError := util.Stringify(nodesInErrorValues)

					nodesWithStaleMonitoringConfig.AddAll(util.GenerifyStringArr(nodesInError))
					monState.StaleNodes, err = util.StringifyInterface(nodesWithStaleMonitoringConfig.GetElements())

					logger.Get().Error("Failed to remove plugin %v with error %v on cluster %v", plugin_name, removeNodeWiseFailure, cluster.Name)
					if removeErr != nil {
						logger.Get().Error("%s-%v", ctxt, removeErr.Error())
					}
					t.UpdateStatus("Failed to remove plugin %v with error %v", plugin_name, removeNodeWiseFailure)
				}
				index := monitoring.GetPluginIndex(plugin_name, cluster.Monitoring.Plugins)
				updatedPlugins := append(cluster.Monitoring.Plugins[:index], cluster.Monitoring.Plugins[index+1:]...)
				monState.Plugins = updatedPlugins
				t.UpdateStatus("Updating the plugin %s removal to db", plugin_name)
				if dbError := updatePluginsInDb(bson.M{"clusterid": uuid}, monState); dbError != nil {
					util.FailTask(fmt.Sprintf("Failed to remove plugin %s for cluster: %v", *uuid, plugin_name), fmt.Errorf("%s-%v", ctxt, dbError), t)
					return
				}
				if len(monState.StaleNodes) == len(cluster_node_names) {
					util.FailTask(fmt.Sprintf("Failed to remove plugin %s for cluster: %v", *uuid, plugin_name), fmt.Errorf("%s-%v", ctxt, removeNodeWiseFailure), t)
					return
				}
				t.UpdateStatus("Success")
				t.Done(models.TASK_STATUS_SUCCESS)
				return
			}
		}
	}
	if taskId, err := a.GetTaskManager().Run(
		models.ENGINE_NAME,
		fmt.Sprintf("Remove monitoring plugin : %s", plugin_name),
		asyncTask,
		nil,
		nil,
		nil); err != nil {
		logger.Get().Error("%s-Unable to create task for remove monitoring plugin for cluster: %v. error: %v", ctxt, *uuid, err)
		HttpResponse(w, http.StatusInternalServerError, "Task creation failed for remove monitoring plugin")
		return
	} else {
		logger.Get().Debug("%s-Task Created: %v for remove monitoring plugin for cluster: %v", ctxt, taskId, *uuid)
		bytes, _ := json.Marshal(models.AsyncResponse{TaskId: taskId})
		w.WriteHeader(http.StatusAccepted)
		w.Write(bytes)
	}
}

func (a *App) GET_MonitoringPlugins(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
	}

	vars := mux.Vars(r)
	cluster_id_str := vars["cluster-id"]
	cluster_id, err := uuid.Parse(cluster_id_str)
	if err != nil {
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Error parsing the cluster id: %s", cluster_id_str))
		logger.Get().Error(fmt.Sprintf("%s-Failed to parse the cluster id with error %v", ctxt, err))
		return
	}
	cluster, err := GetCluster(cluster_id)
	if err != nil {
		if err == mgo.ErrNotFound {
			HttpResponse(w, http.StatusNotFound, fmt.Sprintf("Error getting cluster with id: %v. error: %v", *cluster_id, err))
		} else {
			HttpResponse(w, http.StatusInternalServerError, fmt.Sprintf("Error getting cluster with id: %v. error: %v", *cluster_id, err))
		}
		logger.Get().Error(fmt.Sprintf("%s-Failed to fetch cluster with error %v", ctxt, err))
		return
	}
	if cluster.Name == "" {
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Cluster with id: %v not found", *cluster_id))
		logger.Get().Error("%s-Cluster %v not found", ctxt, *cluster_id)
		return
	} else {
		json.NewEncoder(w).Encode(cluster.Monitoring.Plugins)
	}
}

func (a *App) Get_ClusterSummary(w http.ResponseWriter, r *http.Request) {
	var cSummary models.ClusterSummary
	sessionCopy := db.GetDatastore().Copy()
	defer sessionCopy.Close()
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
		HttpResponse(w, http.StatusInternalServerError, fmt.Sprintf("Error Getting the context.Err %v", err))
		return
	}

	vars := mux.Vars(r)
	cluster_id_str := vars["cluster-id"]
	cluster_id, err := uuid.Parse(cluster_id_str)
	if err != nil {
		logger.Get().Error("%s-Error parsing the cluster id: %s. error: %v", ctxt, cluster_id_str, err)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Error parsing the cluster id: %s", cluster_id_str))
		return
	}

	coll := sessionCopy.DB(conf.SystemConfig.DBConfig.Database).C(models.COLL_NAME_CLUSTER_SUMMARY)
	if err := coll.Find(bson.M{"clusterid": *cluster_id}).One(&cSummary); err != nil {
		if err == mgo.ErrNotFound {
			HttpResponse(w, http.StatusNotFound, fmt.Sprintf("Could not fetch summary.Err %v", err))
		} else {
			HttpResponse(w, http.StatusInternalServerError, fmt.Sprintf("Could not fetch summary.Err %v", err))
		}
		logger.Get().Error(fmt.Sprintf("%s - Could not fetch summary for cluster %v .Err %v", ctxt, *cluster_id, err))
		return
	}
	json.NewEncoder(w).Encode(cSummary)
}

func (a *App) Get_Summary(w http.ResponseWriter, r *http.Request) {
	var system models.System
	sessionCopy := db.GetDatastore().Copy()
	defer sessionCopy.Close()
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
		HttpResponse(w, http.StatusInternalServerError, fmt.Sprintf("Error Getting the context.Err %v", err))
		return
	}
	coll := sessionCopy.DB(conf.SystemConfig.DBConfig.Database).C(models.COLL_NAME_SKYRING_UTILIZATION)
	if err := coll.Find(bson.M{"name": monitoring.SYSTEM}).One(&system); err != nil {
		HttpResponse(w, http.StatusInternalServerError, fmt.Sprintf("Could not fetch summary.Err %v", err))
		logger.Get().Error(fmt.Sprintf("%s - Could not fetch summary.Err %v", ctxt, err))
		return
	}
	json.NewEncoder(w).Encode(system)
}

func fetchThresholdEvents(selectCriteria bson.M, utilizationType string, ctxt string) ([]models.ThresholdEvent, error) {
	var tEventsInDb []models.ThresholdEvent
	sessionCopy := db.GetDatastore().Copy()
	defer sessionCopy.Close()
	coll := sessionCopy.DB(conf.SystemConfig.DBConfig.Database).C(models.COLL_NAME_THRESHOLD_BREACHES)
	err := coll.Find(selectCriteria).All(&tEventsInDb)
	if err != nil {
		if err != mgo.ErrNotFound {
			logger.Get().Warning("%s - Failed to fetch %v events from db.Error %v", ctxt, utilizationType, err)
		}
	}
	return tEventsInDb, err
}

func (a *App) POST_ReSyncClusterSummary(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
		HttpResponse(w, http.StatusInternalServerError, fmt.Sprintf("Error Getting the context.Err %v", err))
		return
	}

	vars := mux.Vars(r)
	cluster_id_str := vars["cluster-id"]
	cluster_id, err := uuid.Parse(cluster_id_str)
	if err != nil {
		logger.Get().Error("%s-Error parsing the cluster id: %s. error: %v", ctxt, cluster_id_str, err)
		HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Error parsing the cluster id: %s", cluster_id_str))
		return
	}

	cluster, err := GetCluster(cluster_id)
	if err != nil {
		if err == mgo.ErrNotFound {
			HttpResponse(w, http.StatusNotFound, fmt.Sprintf("Error getting the cluster with id: %v. error: %v", *cluster_id, err))
		} else {
			HttpResponse(w, http.StatusBadRequest, fmt.Sprintf("Error getting the cluster with id: %v. error: %v", *cluster_id, err))
		}
		logger.Get().Error("%s-Error getting the cluster with id: %v. error: %v", ctxt, *cluster_id, err)
		return

	}

	asyncTask := func(t *task.Task) {
		t.UpdateStatus("Started the task for cluster summary resync: %v", t.ID)
		ComputeClusterSummary(cluster, ctxt)
		t.Done(models.TASK_STATUS_SUCCESS)
	}
	if taskId, err := a.GetTaskManager().Run(models.ENGINE_NAME, fmt.Sprintf("Sync Cluster %s sumary", cluster.Name), asyncTask, nil, nil, nil); err != nil {
		logger.Get().Error("%s-Unable to create task for syncing %s cluster summary. error: %v", ctxt, cluster.Name, err)
		HttpResponse(w, http.StatusInternalServerError, "Task creation failed for sync cluster summary")
		return
	} else {
		logger.Get().Debug("%s-Task Created: %v for syncing %s cluster summary", ctxt, taskId.String(), cluster.Name)
		bytes, _ := json.Marshal(models.AsyncResponse{TaskId: taskId})
		w.WriteHeader(http.StatusAccepted)
		w.Write(bytes)
	}
}

func ComputeClusterSummary(cluster models.Cluster, ctxt string) {
	cSummary := models.ClusterSummary{}

	sessionCopy := db.GetDatastore().Copy()
	defer sessionCopy.Close()

	if mostUsedStorages, err := util.GetTopStorageUsage(bson.M{"clusterid": cluster.ClusterId}); err != nil {
		logger.Get().Error("%s - Failed to fetch most used storages from cluster %v.Err %v", ctxt, cluster.Name, err)
	} else {
		cSummary.MostUsedStorages = mostUsedStorages
	}

	otherProvidersDetails, otherDetailsFetchError := GetApp().FetchClusterDetailsFromProvider(ctxt, cluster.ClusterId)
	if otherDetailsFetchError != nil {
		logger.Get().Warning("%s - Failed to fetch provider specific details for cluster %v.Err : %v", ctxt, cluster.Name, otherDetailsFetchError)
	}
	cSummary.ProviderMonitoringDetails = otherProvidersDetails

	sluStatusWiseCounts, err := util.ComputeSluStatusWiseCount(
		bson.M{"clusterid": cluster.ClusterId},
		bson.M{"utilizationtype": monitoring.SLU_UTILIZATION, "clusterid": cluster.ClusterId, "thresholdseverity": models.CRITICAL})
	if err != nil {
		logger.Get().Error("%s - Failed to fetch storage logical units status wise counts for cluster %v.Err %v", ctxt, cluster.Name, err)
	}
	cSummary.SLUCount = sluStatusWiseCounts

	unmanagedNodes, unmanagedNodesError := GetCoreNodeManager().GetUnmanagedNodes(ctxt)
	if unmanagedNodesError != nil {
		logger.Get().Error("%s - %s", ctxt, fmt.Sprintf("Failed to fetch unmanaged nodes.Err %v", unmanagedNodesError))
	}

	nodesInCluster, clusterNodesFetchError := getClusterNodesById(&(cluster.ClusterId))
	if clusterNodesFetchError != nil {
		logger.Get().Error("%s - %s", ctxt, fmt.Sprintf("Failed to fetch nodes of cluster.Err %v", cluster.Name))
	}
	/*
		Count the number of down nodes
	*/
	var error_nodes int
	var nodeCriticalAlertsCount int
	for _, node := range nodesInCluster {
		nodeCriticalAlertsCount = nodeCriticalAlertsCount + node.AlmCritCount
		if node.Status == models.NODE_STATUS_ERROR {
			error_nodes = error_nodes + 1
		}
	}

	cSummary.NodesCount = map[string]int{models.TOTAL: len(nodesInCluster), models.NodeStatuses[models.NODE_STATUS_ERROR]: error_nodes, models.NodeStates[models.NODE_STATE_UNACCEPTED]: len(*unmanagedNodes), "criticalAlerts": nodeCriticalAlertsCount}

	cSummary.Usage = cluster.Usage
	cSummary.ObjectCount = cluster.ObjectCount
	cSummary.Utilizations = cluster.Utilizations

	storageProfileUtilization := make(map[string]map[string]interface{})
	pluginIndex := monitoring.GetPluginIndex(monitoring.STORAGE_PROFILE_UTILIZATION, cluster.Monitoring.Plugins)
	if pluginIndex != -1 {
		if storageProfileUtilization, err = util.ComputeStorageProfileUtilization(
			bson.M{"clusterid": cluster.ClusterId},
			cluster.Monitoring.Plugins[pluginIndex].Configs); err != nil {
			logger.Get().Error("%s - Failed to fetch storage profile utilization of cluster %v.Error %v", ctxt, cluster.Name, err)
		}
	}
	cSummary.StorageProfileUsage = storageProfileUtilization

	storageCount, err := util.GetStorageCount(bson.M{"clusterid": cluster.ClusterId})
	if err != nil {
		logger.Get().Error("%s - Failed to fetch storage status wise counts for cluster %v.Error %v", ctxt, cluster.Name, err)
	}
	cSummary.StorageCount = storageCount

	cSummary.ClusterId = cluster.ClusterId
	cSummary.Type = cluster.Type
	cSummary.Name = cluster.Name
	cSummary.MonitoringPlugins = cluster.Monitoring.Plugins
	cSummary.UpdatedAt = time.Now().String()

	coll := sessionCopy.DB(conf.SystemConfig.DBConfig.Database).C(models.COLL_NAME_CLUSTER_SUMMARY)
	if _, err := coll.Upsert(bson.M{"clusterid": cluster.ClusterId}, cSummary); err != nil {
		logger.Get().Error("%s - Error persisting the cluster summary.Error %v", ctxt, err)
	}
}

func (a *App) POST_ReSyncSystemSummary(w http.ResponseWriter, r *http.Request) {
	ctxt, err := GetContext(r)
	if err != nil {
		logger.Get().Error("Error Getting the context. error: %v", err)
		HttpResponse(w, http.StatusInternalServerError, fmt.Sprintf("Error Getting the context.Err %v", err))
		return
	}
	asyncTask := func(t *task.Task) {
		t.UpdateStatus("Started the task for system summary resync: %v", t.ID)
		ComputeSystemSummary(make(map[string]interface{}))
		t.Done(models.TASK_STATUS_SUCCESS)
	}
	if taskId, err := a.GetTaskManager().Run(models.ENGINE_NAME, fmt.Sprintf("Sync system sumary"), asyncTask, nil, nil, nil); err != nil {
		logger.Get().Error("%s-Unable to create task for syncing system summary. error: %v", ctxt, err)
		HttpResponse(w, http.StatusInternalServerError, "Task creation failed for sync system summary")
		return
	} else {
		logger.Get().Debug("%s-Task Created: %v for syncing system summary", ctxt, taskId.String())
		bytes, _ := json.Marshal(models.AsyncResponse{TaskId: taskId})
		w.WriteHeader(http.StatusAccepted)
		w.Write(bytes)
	}
}

func ComputeSystemSummary(p map[string]interface{}) {
	var system models.System

	reqId, err := uuid.New()
	if err != nil {
		logger.Get().Error("Error Creating the RequestId. error: %v", err)
		return
	}

	ctxt := fmt.Sprintf("%v:%v", models.ENGINE_NAME, reqId.String())

	system.Name = monitoring.SYSTEM
	time_stamp_str := strconv.FormatInt(time.Now().Unix(), 10)
	table_name := conf.SystemConfig.TimeSeriesDBConfig.CollectionName + "." + models.SYSTEM + "."
	hostname := conf.SystemConfig.TimeSeriesDBConfig.Hostname
	port := conf.SystemConfig.TimeSeriesDBConfig.DataPushPort

	clusters, clusterFetchError := GetClusters()
	if clusterFetchError != nil {
		if clusterFetchError == mgo.ErrNotFound {
			return
		}
		logger.Get().Error("%s - Failed to fetch clusters.Err %v", ctxt, clusterFetchError)
	}

	sessionCopy := db.GetDatastore().Copy()
	defer sessionCopy.Close()

	if system.NodesCount, err = util.FetchNodeStatusWiseCounts(nil); err != nil {
		logger.Get().Error("%s - Failed to fetch status wise node counts.Error %v", ctxt, err)
	}

	sluStatusWiseCounts, err := util.ComputeSluStatusWiseCount(nil, bson.M{"utilizationtype": monitoring.SLU_UTILIZATION, "thresholdseverity": models.CRITICAL})
	if err != nil {
		logger.Get().Error("%s - Error getting the slus list. error: %v", ctxt, err)
	}
	system.SLUCount = sluStatusWiseCounts

	storageCount, err := util.GetStorageCount(nil)
	if err != nil {
		logger.Get().Error("%s - Failed to fetch storage status wise counts.Error %v", ctxt, err)
	}
	system.StorageCount = storageCount

	var net_cluster_used int64
	var net_cluster_total int64
	var net_memory_used float64
	net_memory_used_count := len(clusters)
	var net_memory_total float64
	net_memory_total_count := len(clusters)
	var cluster_cpu_user float64
	cluster_cpu_user_count := len(clusters)
	var latency float64
	latencyCount := len(clusters)
	var netIStatRx float64
	netIStatRxCount := len(clusters)
	var netIStatTx float64
	netIStatTxCount := len(clusters)
	var netDiskRead float64
	netDiskReadCount := len(clusters)
	var netDiskWrite float64
	netDiskWriteCount := len(clusters)

	for _, cluster := range clusters {

		ComputeClusterSummary(cluster, ctxt)

		/*
			Calculate net cluster utilization
		*/
		net_cluster_used = net_cluster_used + cluster.Usage.Used
		net_cluster_total = net_cluster_total + cluster.Usage.Total

		/*
			Calculate Memory Used
		*/
		resource_name := monitoring.MEMORY + "-" + monitoring.USED_SPACE
		net_memory_used = net_memory_used + FetchStatFromGraphite(ctxt, cluster.Name, resource_name, &net_memory_used_count)

		/*
			Calculate Free Memory
		*/
		resource_name = monitoring.MEMORY + "-" + monitoring.TOTAL_SPACE
		net_memory_total = net_memory_total + FetchStatFromGraphite(ctxt, cluster.Name, resource_name, &net_memory_total_count)

		/*
			Calculate cpu user utilization
		*/
		resource_name = monitoring.CPU_USER
		if cpuPercentageUsage, ok := cluster.Utilizations["cpupercentageusage"]; ok {
			cluster_cpu_user = cluster_cpu_user + cpuPercentageUsage.(float64)
		} else {
			cluster_cpu_user_count = cluster_cpu_user_count - 1
		}

		/*
			Calculate Latency
		*/
		resource_name = monitoring.NETWORK_LATENCY
		latency = latency + FetchStatFromGraphite(ctxt, cluster.Name, resource_name, &latencyCount)

		// Aggregate disk read
		resource_name = monitoring.DISK + "-" + monitoring.READ
		netDiskRead = netDiskRead + FetchStatFromGraphite(ctxt, cluster.Name, resource_name, &netDiskReadCount)

		// Aggregate disk write
		resource_name = monitoring.DISK + "-" + monitoring.WRITE
		netDiskWrite = netDiskWrite + FetchStatFromGraphite(ctxt, cluster.Name, resource_name, &netDiskWriteCount)

		// Aggregate interface rx
		resource_name = monitoring.INTERFACE + "-" + monitoring.RX
		netIStatRx = netIStatRx + FetchStatFromGraphite(ctxt, cluster.Name, resource_name, &netIStatRxCount)

		// Aggregate interface tx
		resource_name = monitoring.INTERFACE + "-" + monitoring.TX
		netIStatTx = netIStatTx + FetchStatFromGraphite(ctxt, cluster.Name, resource_name, &netIStatTxCount)

	}

	clustersCount, err := util.ComputeClustersStatusWiseCounts()
	if err != nil {
		logger.Get().Error("%s - Error getting  status wise clusters count.Error %v", ctxt, err)
	}
	system.ClustersCount = clustersCount

	// Update Cluster utilization to time series db
	var percentSystemUsed float64
	if net_cluster_total != 0 {
		percentSystemUsed = (float64(net_cluster_used*100) / float64(net_cluster_total))
	}
	system.Usage = models.Utilization{Used: net_cluster_used, Total: net_cluster_total, PercentUsed: percentSystemUsed, UpdatedAt: time.Now().String()}
	if net_cluster_total != 0 {
		if err := GetMonitoringManager().PushToDb(map[string]map[string]string{fmt.Sprintf("%s%s.%s", table_name, monitoring.SYSTEM_UTILIZATION, monitoring.USED_SPACE): {time_stamp_str: strconv.FormatInt(system.Usage.Used, 10)}}, hostname, port); err != nil {
			logger.Get().Warning("%s - Error pushing cluster utilization.Err %v", ctxt, err)
		}
		if err := GetMonitoringManager().PushToDb(map[string]map[string]string{fmt.Sprintf("%s%s.%s", table_name, monitoring.SYSTEM_UTILIZATION, monitoring.TOTAL_SPACE): {time_stamp_str: strconv.FormatInt(system.Usage.Total, 10)}}, hostname, port); err != nil {
			logger.Get().Warning("%s - Error pushing cluster utilization.Err %v", ctxt, err)
		}
		if err := GetMonitoringManager().PushToDb(map[string]map[string]string{fmt.Sprintf("%s%s.%s", table_name, monitoring.SYSTEM_UTILIZATION, monitoring.PERCENT_USED): {time_stamp_str: strconv.FormatFloat(percentSystemUsed, 'E', -1, 64)}}, hostname, port); err != nil {
			logger.Get().Warning("%s - Error pushing cluster utilization.Err %v", ctxt, err)
		}
	}

	systemthresholds := monitoring.GetSystemDefaultThresholdValues()
	net_storage_profile_utilization, err := util.ComputeStorageProfileUtilization(nil, systemthresholds[monitoring.STORAGE_PROFILE_UTILIZATION].Configs)
	if err != nil {
		logger.Get().Error("%s - Failed to get storge profile utilization.Error %v", ctxt, err)
	}
	system.StorageProfileUsage = net_storage_profile_utilization

	system.ProviderMonitoringDetails = make(map[string]map[string]interface{})
	var cpuPercentUsed float64
	if cluster_cpu_user_count > 0 {
		cpuPercentUsed = float64(cluster_cpu_user) / float64(cluster_cpu_user_count)
	}
	system.MonitoringPlugins = systemthresholds
	if len(clusters) > 0 {
		otherProvidersDetails, otherDetailsFetchError := GetApp().FetchMonitoringDetailsFromProviders(ctxt)
		if otherDetailsFetchError != nil {
			logger.Get().Error("%s - Error fetching the provider specific details. Error %v", ctxt, otherDetailsFetchError)
		}
		system.ProviderMonitoringDetails = otherProvidersDetails
		netDiskRead = AverageAndUpdateDb(ctxt, netDiskRead, netDiskReadCount, time_stamp_str, table_name+monitoring.DISK+"-"+monitoring.READ)
		netDiskWrite = AverageAndUpdateDb(ctxt, netDiskWrite, netDiskWriteCount, time_stamp_str, table_name+monitoring.DISK+"-"+monitoring.WRITE)
		if netDiskReadCount > 0 && netDiskWriteCount > 0 {
			UpdateMetricToTimeSeriesDb(ctxt, netDiskRead+netDiskWrite, time_stamp_str, fmt.Sprintf("%s%s-%s_%s", table_name, monitoring.DISK, monitoring.READ, monitoring.WRITE))
		}

		if net_memory_total > 0.0 {
			UpdateMetricToTimeSeriesDb(ctxt, net_memory_used, time_stamp_str, table_name+monitoring.MEMORY+"-"+monitoring.USED_SPACE)
			UpdateMetricToTimeSeriesDb(ctxt, net_memory_total, time_stamp_str, table_name+monitoring.MEMORY+"-"+monitoring.TOTAL_SPACE)
		}

		AverageAndUpdateDb(ctxt, cluster_cpu_user, cluster_cpu_user_count, time_stamp_str, fmt.Sprintf("%s%s-%s", table_name, monitoring.CPU_USER, monitoring.CPU_SYSTEM))
		AverageAndUpdateDb(ctxt, latency, latencyCount, time_stamp_str, table_name+monitoring.NETWORK_LATENCY)

		netIStatRx = AverageAndUpdateDb(ctxt, netIStatRx, netIStatRxCount, time_stamp_str, table_name+monitoring.INTERFACE+"-"+monitoring.RX)
		netIStatTx = AverageAndUpdateDb(ctxt, netIStatTx, netIStatTxCount, time_stamp_str, table_name+monitoring.INTERFACE+"-"+monitoring.TX)
		if netIStatRxCount > 0 && netIStatTxCount > 0 {
			UpdateMetricToTimeSeriesDb(ctxt, netIStatRx+netIStatTx, time_stamp_str, fmt.Sprintf("%s%s-%s_%s", table_name, monitoring.INTERFACE, monitoring.RX, monitoring.TX))
		}

		var memory_percent float64
		if net_memory_total > 0.0 {
			memory_percent = (net_memory_used * 100) / net_memory_total
			memory_percent_table := table_name + monitoring.MEMORY + "-" + monitoring.USAGE_PERCENT
			if err := GetMonitoringManager().PushToDb(map[string]map[string]string{memory_percent_table: {time_stamp_str: strconv.FormatFloat(memory_percent, 'E', -1, 64)}}, hostname, port); err != nil {
				logger.Get().Warning("%s - Error pushing memory utilization.Err %v", ctxt, err)
			}
		}

		systemUtilizations := map[string]interface{}{
			"memoryusage": models.Utilization{
				Used:        int64(net_memory_used),
				Total:       int64(net_memory_total),
				PercentUsed: memory_percent,
				UpdatedAt:   time.Now().String(),
			},
			"cpupercentageusage": cpuPercentUsed,
		}
		system.Utilizations = systemUtilizations
	}
	system.UpdatedAt = time.Now().String()

	mostUsedStorages, err := util.GetTopStorageUsage(nil)
	if err != nil {
		logger.Get().Error("%s - Failed to get most used storages.Error %v", ctxt, err)
	}
	system.MostUsedStorages = mostUsedStorages
	/*
		Persist system into db
	*/
	coll := sessionCopy.DB(conf.SystemConfig.DBConfig.Database).C(models.COLL_NAME_SKYRING_UTILIZATION)
	if _, err := coll.Upsert(bson.M{"name": monitoring.SYSTEM}, system); err != nil {
		logger.Get().Error("%s - Error persisting the system.Error %v", ctxt, err)
	}
}
