/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#include <python/bindings.h>

#include <sstream>
#include "data/constructs/PythonIterInfo.h"
#include "data/constructs/IterInfo.h"
#include "data/constructs/security/Permissions.h"
#include "data/constructs/KeyValue.h"
#include "data/constructs/Mutation.h"
#include "data/constructs/security/Authorizations.h"
#include "scanner/constructs/Results.h"
#include "scanner/impl/Scanner.h"
#include "scanner/constructs/Results.h"
#include "writer/impl/SinkImpl.h"
#include "data/constructs/client/zookeeperinstance.h"
#include "interconnect/Accumulo.h"
#include "interconnect/tableOps/TableOperations.h"
#include "interconnect/python/PythonStructures.h"
#include "interconnect/namespaceOps/NamespaceOperations.h"
#include "data/constructs/rfile/RFile.h"
#include "data/constructs/rfile/SequentialRFile.h"
#include "data/constructs/compressor/compressor.h"
#include "data/constructs/compressor/zlibCompressor.h"
#include "../include/logging/Logger.h"
#include "../include/logging/LoggerConfiguration.h"
#include "data/constructs/rfile/RFile.h"
#include "data/constructs/rfile/RFileOperations.h"
#include "data/iterators/MultiIterator.h"
#include "data/streaming/accumulo/KeyValueIterator.h"
#include "data/constructs/client/Hdfs.h"
#include "data/streaming/accumulo/StreamSeekable.h"
#include "data/streaming/StreamRelocation.h"
#include "scanner/ScannerOptions.h"
#include "data/streaming/HdfsOutputStream.h"
#include "data/streaming/input/HdfsInputStream.h"
#include "data/constructs/coordinator/AccumuloInfo.h"
#include "data/constructs/coordinator/TableInfo.h"
#include "data/constructs/coordinator/TabletServerStatus.h"
using namespace pybind11::literals;

PYBIND11_DECLARE_HOLDER_TYPE(T, std::shared_ptr<T>);

PYBIND11_MODULE(pysharkbite, s) {
  s.doc() = "Accumulo connector plugin";

  pybind11::class_<cclient::impl::Configuration, std::shared_ptr<cclient::impl::Configuration>>(s, "Configuration" , "Sharkbite Configuration object.")
  .def(pybind11::init<>())
  .def("set", &cclient::impl::Configuration::set, "Set a configuration option")
  .def("get", (std::string (cclient::impl::Configuration::*)(const std::string &) const) &cclient::impl::Configuration::get, "Get the configuration option")
  .def("get", (std::string (cclient::impl::Configuration::*)(const std::string &, const std::string &) const) &cclient::impl::Configuration::get, "Get the configuration option with default value")
  .def("getLong", (uint32_t (cclient::impl::Configuration::*)(const std::string &, uint32_t) const) &cclient::impl::Configuration::getLong, "Get the integer value of a configuration item");

  pybind11::class_<cclient::data::Instance, std::shared_ptr<cclient::data::Instance>>(s, "Instance" , "Accumulo Instance");
  pybind11::class_<logging::LoggerConfiguration>(s, "LoggingConfiguration", "Sharkbite Logger, enables logging at the debug and trace levels")
  .def_static("enableDebugLogger",&logging::LoggerConfiguration::enableLogger, "Enables the debug logging for all classes")
  .def_static("enableTraceLogger",&logging::LoggerConfiguration::enableTraceLogger, "Enables trace logging for all classes");

  pybind11::class_<cclient::data::zookeeper::ZookeeperInstance, std::shared_ptr<cclient::data::zookeeper::ZookeeperInstance>, cclient::data::Instance>(s, "ZookeeperInstance", "Zookeeper instance enables connectivity to a zookeper quorum")
  .def(pybind11::init<std::string, std::string,uint32_t, const std::shared_ptr<cclient::impl::Configuration>&>())
  .def("getInstanceName", &cclient::data::zookeeper::ZookeeperInstance::getInstanceName)
  .def("getInstanceId", &cclient::data::zookeeper::ZookeeperInstance::getInstanceId,"retry"_a=false);

  pybind11::class_<cclient::data::security::AuthInfo>(s, "AuthInfo", "Auth info object contains accessor to an Accumulo Instance")
  .def(pybind11::init<std::string, std::string,std::string>())
  .def("getUserName",&cclient::data::security::AuthInfo::getUserName, "Get the username")
  .def("getPassword", &cclient::data::security::AuthInfo::getPassword, "Get the user's password")
  .def("getInstanceId", &cclient::data::security::AuthInfo::getInstanceId, "Get the instance ID");

  pybind11::class_<cclient::data::IterInfo>(s, "IterInfo", "IterInfo is an iterator configuration")
  .def(pybind11::init<const std::string &,const std::string &, uint32_t>())
  .def(pybind11::init<const std::string &,const std::string &,uint32_t,const std::string &>(),
      "script"_a, "iteratorName"_a,"priority"_a,"type"_a="Python")
  .def("getPriority",&cclient::data::IterInfo::getPriority, "Get the priority for this iterator")
  .def("getName",&cclient::data::IterInfo::getName, "Get the name of this iterator")
  .def("getClass",&cclient::data::IterInfo::getClass, "Get the class for this iterator");

  pybind11::class_<cclient::data::python::PythonIterInfo>(s, "PythonIterator", "Defines a python iterator")
  .def(pybind11::init<const std::string &,const std::string &, uint32_t>())
  .def(pybind11::init<const std::string &,uint32_t>())
  .def("getPriority",&cclient::data::python::PythonIterInfo::getPriority,"Get the priority for this iterator")
  .def("getName",&cclient::data::python::PythonIterInfo::getName,"Get the name of this iterator")
  .def("onNext",&cclient::data::python::PythonIterInfo::onNext, "Lambda that is provided the accumulo key")
  .def("getClass",&cclient::data::python::PythonIterInfo::getClass,"Get the class for this iterator");



  pybind11::class_<cclient::data::TableRates>(s, "TableRates", pybind11::dynamic_attr())
    .def("getIngestRate",&cclient::data::TableRates::getIngestRate, "Gets the ingest rate in records.")
    .def_readonly("query_rate_byte",&cclient::data::TableRates::queryByteRate)
    .def("getIngestRateByte",&cclient::data::TableRates::getIngestRateByte, "Gets the ingest rate in bytes.")
    .def_readonly("query_rate_byte",&cclient::data::TableRates::queryByteRate)
    .def("getQueryRate",&cclient::data::TableRates::getQueryRate, "Gets the query rate in records.")
    .def_readonly("query_rate_byte",&cclient::data::TableRates::queryByteRate)
    .def("getQueryRateByte",&cclient::data::TableRates::getQueryRateByte, "Gets the query rate in bytes.")
    .def_readonly("query_rate_byte",&cclient::data::TableRates::queryByteRate)
    .def("getScanRate",&cclient::data::TableRates::getScanRate, "Gets the scan rate.")
    .def_readonly("scan_rate",&cclient::data::TableRates::scanRate);


  pybind11::class_<cclient::data::Compacting>(s, "Compacting", pybind11::dynamic_attr())
    .def("getRunning",&cclient::data::Compacting::getRunning, "Gets running compactions.")
    .def_readonly("running",&cclient::data::Compacting::running)
    .def("getQueued",&cclient::data::Compacting::getQueued, "Gets queued compactions.")
    .def_readonly("queued",&cclient::data::Compacting::queued);

    
    pybind11::class_<cclient::data::RecoveryStatus>(s, "RecoveryStatus", pybind11::dynamic_attr())
    .def("getName",&cclient::data::RecoveryStatus::getName, "Gets the name of the recovery.")
    .def_readonly("name",&cclient::data::RecoveryStatus::name)
    .def("getRuntime",&cclient::data::RecoveryStatus::getRuntime, "Gets the runtime of the recovery process.")
    .def_readonly("runtime",&cclient::data::RecoveryStatus::runtime)
    .def("getProgress",&cclient::data::RecoveryStatus::getProgress, "Gets the progress of the recovery.")
    .def_readonly("progress",&cclient::data::RecoveryStatus::progress);

    pybind11::class_<cclient::data::TableCompactions>(s, "TableCompactions", pybind11::dynamic_attr())
    .def("getMinorCompactions",&cclient::data::TableCompactions::getMinorCompactions, "Gets minor compaction stats.")
    .def_readonly("minors",&cclient::data::TableCompactions::minors)
    .def("getMajorCompactions",&cclient::data::TableCompactions::getMajorCompactions, "Gets major compaction stats.")
    .def_readonly("majors",&cclient::data::TableCompactions::majors)
    .def("getScans",&cclient::data::TableCompactions::getScans, "Gets scan stats.")
    .def_readonly("scans",&cclient::data::TableCompactions::scans);

    pybind11::class_<cclient::data::TableInfo>(s, "TableInfo", pybind11::dynamic_attr())
    .def("getRecords",&cclient::data::TableInfo::getRecords, "Gets records in the table")
    .def_readonly("records",&cclient::data::TableInfo::recs)
    .def("getRecordsInMemory",&cclient::data::TableInfo::getRecordsInMemory, "Gets records in memory for a table")
    .def_readonly("records_in_memory",&cclient::data::TableInfo::recsInMemory)
    .def("getTablets",&cclient::data::TableInfo::getTablets, "Gets tablets in the table")
    .def_readonly("tablets",&cclient::data::TableInfo::tablets)
    .def("getOnlineTablets",&cclient::data::TableInfo::getOnlineTablets, "Gets online tablets in the table")
    .def_readonly("online_tables",&cclient::data::TableInfo::onlineTablets)
    .def("getTableRates",&cclient::data::TableInfo::getTableRates, "Gets table rates for the table")
    .def_readonly("table_rates",&cclient::data::TableInfo::rates)
    .def("getCompactioninfo",&cclient::data::TableInfo::getCompactioninfo, "Gets compaction info for the table")
    .def_readonly("compaction_info",&cclient::data::TableInfo::compactionInfo);

  pybind11::class_<cclient::data::TabletServerStatus>(s, "TabletServerStatus", pybind11::dynamic_attr())
    .def("getTableMap",&cclient::data::TabletServerStatus::getTableMap, "Gets the table map")
    .def_readonly("table_map",&cclient::data::TabletServerStatus::tableMap)
    .def("getLastContact",&cclient::data::TabletServerStatus::getLastContact, "Gets the last contact time of the server")
    .def_readonly("last_contact",&cclient::data::TabletServerStatus::lastContact)
    .def("getName",&cclient::data::TabletServerStatus::getName, "Gets the name of the server")
    .def_readonly("name",&cclient::data::TabletServerStatus::name)
    .def("getOsLoad",&cclient::data::TabletServerStatus::getOsLoad, "Gets the load of the server")
    .def_readonly("os_load",&cclient::data::TabletServerStatus::osLoad)
    .def("getLookups",&cclient::data::TabletServerStatus::getLookups, "Gets lookups against the server")
    .def_readonly("lookups",&cclient::data::TabletServerStatus::lookups)
    .def("getIndexCacheHits",&cclient::data::TabletServerStatus::getIndexCacheHits, "Gets index cache hits against the server")
    .def_readonly("index_cache_hits",&cclient::data::TabletServerStatus::indexCacheHits)
    .def("getDataCacheHits",&cclient::data::TabletServerStatus::getDataCacheHits, "Gets data cache hits against the server")
    .def_readonly("data_cache_hits",&cclient::data::TabletServerStatus::dataCacheHits)
    .def("getDataCacheRequests",&cclient::data::TabletServerStatus::getDataCacheRequests, "Gets data cache requests against the server")
    .def_readonly("data_cache_requests",&cclient::data::TabletServerStatus::dataCacheRequest)
    .def("getLogSorts",&cclient::data::TabletServerStatus::getLogSorts, "Gets the number of log sorts")
    .def_readonly("log_sorts",&cclient::data::TabletServerStatus::logSorts)
    .def("getFlushes",&cclient::data::TabletServerStatus::getFlushes, "Gets the number of flushes on the server")
    .def_readonly("flushes",&cclient::data::TabletServerStatus::flushs)
    .def("getSyncs",&cclient::data::TabletServerStatus::getSyncs, "Gets the number of syncs on the server")
    .def_readonly("syncs",&cclient::data::TabletServerStatus::syncs)
    .def("getHoldTime",&cclient::data::TabletServerStatus::getHoldTime, "Gets the hold time")
    .def_readonly("hold_time",&cclient::data::TabletServerStatus::holdTime);



   pybind11::class_<cclient::data::DeadServer>(s, "DeadServer", pybind11::dynamic_attr())
    .def("getServer",&cclient::data::DeadServer::getServer, "Gets the server name.")
    .def_readonly("server",&cclient::data::DeadServer::server)
    .def("getLastContact",&cclient::data::DeadServer::getLastContact, "Get last contact time with this server.")
    .def_readonly("last_contact",&cclient::data::DeadServer::lastStatus)
    .def("getStatus",&cclient::data::DeadServer::getStatus, "Gets the status of the server.")
    .def_readonly("status",&cclient::data::DeadServer::status);;

  pybind11::enum_<cclient::data::CoordinatorGoalState::type>(s, "CoordinatorGoalState", pybind11::arithmetic())
    .value("CLEAN_STOP", cclient::data::CoordinatorGoalState::type::CLEAN_STOP, "CLEAN_STOP state")
    .value("SAFE_MODE", cclient::data::CoordinatorGoalState::type::SAFE_MODE, "SAFE_MODE state")
    .value("NORMAL", cclient::data::CoordinatorGoalState::type::NORMAL, "NORMAL state");


  pybind11::enum_<cclient::data::CoordinatorState::type>(s, "CoordinatorState", pybind11::arithmetic())
    .value("INITIAL", cclient::data::CoordinatorState::type::INITIAL, "INITIAL state")
    .value("HAVE_LOCK", cclient::data::CoordinatorState::type::HAVE_LOCK, "HAVE_LOCK state")
    .value("SAFE_MODE", cclient::data::CoordinatorState::type::SAFE_MODE, "SAFE_MODE state")
    .value("NORMAL", cclient::data::CoordinatorState::type::NORMAL, "NORMAL state")
    .value("UNLOAD_METADATA_TABLETS", cclient::data::CoordinatorState::type::UNLOAD_METADATA_TABLETS, "UNLOAD_METADATA_TABLETS state")
    .value("UNLOAD_ROOT_TABLET", cclient::data::CoordinatorState::type::UNLOAD_ROOT_TABLET, "UNLOAD_ROOT_TABLET state")
    .value("STOP", cclient::data::CoordinatorState::type::STOP, "STOP state");

  pybind11::class_<cclient::data::AccumuloInfo>(s, "AccumuloInfo", pybind11::dynamic_attr())
    .def("getTableMap",&cclient::data::AccumuloInfo::getTableMap, "Gets the Table map for the cluster.")
    .def_readonly("table_map",&cclient::data::AccumuloInfo::tableMap)
    .def("getTabletServerInfo",&cclient::data::AccumuloInfo::getTabletServerInfo, "Gets tablet server Info")
    .def_readonly("tablet_server_info",&cclient::data::AccumuloInfo::tServerInfo)
    .def("getBadTabletServers",&cclient::data::AccumuloInfo::getBadTabletServers, "Gets a mapping of bad tablet servers.")
    .def_readonly("bad_servers",&cclient::data::AccumuloInfo::badTServers)
    .def("getState",&cclient::data::AccumuloInfo::getState, "Gets the state of the accumulo cluster.")
    .def_readonly("state",&cclient::data::AccumuloInfo::state)
    .def("getGoalState",&cclient::data::AccumuloInfo::getGoalState, "Returns the goal state of the cluster.")
    .def_readonly("goal_state",&cclient::data::AccumuloInfo::goalState)
    .def("getUnassignedTablets",&cclient::data::AccumuloInfo::getUnassignedTablets, "Gets the unassigned tablets")
    .def_readonly("unassigned_tablets",&cclient::data::AccumuloInfo::unassignedTablets)
    .def("getServerShuttingDown",&cclient::data::AccumuloInfo::getServerShuttingDown, "Returns the set of servers shutting down.")
    .def_readonly("servs_shutting_down",&cclient::data::AccumuloInfo::serversShuttingDown)
    .def("getDeadServers",&cclient::data::AccumuloInfo::getDeadServers, "Returns a list of dead tablet servers.")
    .def_readonly("dead_servers",&cclient::data::AccumuloInfo::deadTabletServers);



 pybind11::class_<interconnect::PythonNamespaceOperations>(s, "AccumuloNamespaceOperations", "Accumulo namespace operations. Should be accessed through 'AccumuloConnector'")
  .def("list",&interconnect::PythonNamespaceOperations::list, "Lists namespaces within this Accumulo instance")
  .def("remove",&interconnect::PythonNamespaceOperations::remove, "removes the namespace")
  .def("exists",&interconnect::PythonNamespaceOperations::exists, "Returns true if the namespace exists")
  .def("rename",&interconnect::PythonNamespaceOperations::rename, "Renames the namespace")
  .def("setProperty", &interconnect::PythonNamespaceOperations::setProperty, "Sets a namespace property")
  .def("removeProperty", &interconnect::PythonNamespaceOperations::removeProperty, "Remove the namespace property")
  .def("create",&interconnect::PythonNamespaceOperations::create, "Creates the namespace");

  pybind11::class_<interconnect::PythonTableOperations>(s, "AccumuloTableOperations", "Accumulo table operations. Should be accessed through 'AccumuloConnector'")
  .def("remove",&interconnect::PythonTableOperations::remove, "remove the table")
  .def("exists",&interconnect::PythonTableOperations::exists, "Returns true if the table exists")
  .def("import",&interconnect::PythonTableOperations::import, "import data into this directory")
  .def("flush",&interconnect::PythonTableOperations::flush, "flush the table")
  .def("compact", &interconnect::PythonTableOperations::compact, "compact the table")
  .def("setProperty", &interconnect::PythonTableOperations::setProperty, "Set table property")
  .def("removeProperty", &interconnect::PythonTableOperations::removeProperty, "Remove the table property")
  .def("addSplits", &interconnect::PythonTableOperations::addSplits, "Add splits for a table")
  .def("addConstraint", &interconnect::PythonTableOperations::addConstraint, "Add table constraint")
  .def("createScanner", &interconnect::PythonTableOperations::createScanner, "createWriter scanner")
  .def("createWriter", &interconnect::PythonTableOperations::createWriter, "Create writer for table")
  .def("create",&interconnect::PythonTableOperations::create, "Create the table");

  pybind11::class_<interconnect::PythonTableInfo>(s, "AccumuloTableInfo", "Provides you table information")
  .def("table_id",&interconnect::PythonTableInfo::getTableId, "Returns the table Id for the corresponding table")
  .def("table_name",&interconnect::PythonTableInfo::getTableName, "Gets the table name for the corresponding table Id")
  .def("exists",&interconnect::PythonTableInfo::exists, "Returns true if the table exists");

    pybind11::class_<interconnect::AccumuloConnector,  std::shared_ptr<interconnect::AccumuloConnector>>(s, "AccumuloConnector", "Accumulo connector")
  .def(pybind11::init<cclient::data::security::AuthInfo&, std::shared_ptr<cclient::data::Instance>>())
  .def("securityOps",&interconnect::AccumuloConnector::security_operations, "Return the security operations object")
  .def("namespaceOps",&interconnect::AccumuloConnector::namespace_operations, "Allows the user to perform namespace operations")
  .def("getStatistics",&interconnect::AccumuloConnector::getStatistics, "Returns Statistics for the accumulo connector")
  .def("tableOps",&interconnect::AccumuloConnector::table_operations, "Return the table operations object")
  .def("tableInfo",&interconnect::AccumuloConnector::table_info, "Return the table(s) Info objects ");

  pybind11::class_<cclient::data::Value, std::shared_ptr<cclient::data::Value>>(s, "Value", "Accumulo value")
  .def(pybind11::init<>())
  .def("get", &cclient::data::Value::getValueAsString, "Returns the value as a UTF-8 string")
  .def("get_bytes",
      [](cclient::data::Value &self) {
          return pybind11::bytes(self.getValueAsString());  // Return the data without transcoding
      }, "Returns the bytes as python bytes"
  )
  .def("__repr__",[](const std::shared_ptr<cclient::data::Value> &self) {
        if (self){  
          return self->getValueAsString();
        }
        else{
          return std::string("[]");
        }
    })
  .def("__str__",[](const std::shared_ptr<cclient::data::Value> &self) {
        if (self){  
          return self->getValueAsString();
        }
        else{
          return std::string("");
        }
    });

  pybind11::class_<cclient::data::Key, std::shared_ptr<cclient::data::Key>>(s, "Key", "Accumulo Key")
  .def(pybind11::init<>())
  .def(pybind11::init<const char *,const char *, const char *, const char *, int64_t>(),
      "row"_a, "columnfamily"_a=nullptr,"columnqualifier"_a=nullptr,"columnvisibility"_a=nullptr,"timestamp"_a=9223372036854775807L)
  .def("setRow",(void (cclient::data::Key::*)(const std::string &) ) &cclient::data::Key::setRow, "Sets the row")
  .def("__str__",[](const std::shared_ptr<cclient::data::Key> &si) {
        if (si){
          return si->toString();
        }
        else{
          return std::string(" : []");
        }
    })
  .def("__repr__",[](const std::shared_ptr<cclient::data::Key> &si) {
        if (si){
          return si->toString();
        }
        else{
          return std::string(" : []");
        }
    })
  .def("setColumnFamily",(void (cclient::data::Key::*)(const std::string &) ) &cclient::data::Key::setColFamily, "Sets the column fmaily")
  .def("setColumnQualifier",(void (cclient::data::Key::*)(const std::string &) ) &cclient::data::Key::setColQualifier, "Sets the column qualifier")
  .def("getRow", &cclient::data::Key::getRowStr, "Gets the row")
  .def("getColumnFamily", &cclient::data::Key::getColFamilyStr, "Gets the Column Family")
  .def("getColumnVisibility", &cclient::data::Key::getColVisibilityStr, "Gets the Column Visibility")
  .def("getTimestamp", &cclient::data::Key::getTimeStamp, "Gets the key timestamp")
  .def("getColumnQualifier", &cclient::data::Key::getColQualifierStr, "Gets the Column Qualifier");

  pybind11::class_<cclient::data::KeyValue, std::shared_ptr<cclient::data::KeyValue>>(s, "KeyValue")
  .def(pybind11::init<>())
  .def(pybind11::init<const std::shared_ptr<cclient::data::Key> &, const std::shared_ptr<cclient::data::Value> &>())
  .def("getKey", &cclient::data::KeyValue::getKey, "Gets the Key from the key value object")
  .def("getValue", &cclient::data::KeyValue::getValue, "Gets the Value from the key value object");

  pybind11::class_<cclient::data::Range>(s, "Range")
    .def(pybind11::init<>(), "Constructor with an infinite range")
  .def(pybind11::init<const std::string&>(), "Constructor that accepts a string range")
  .def(pybind11::init<std::shared_ptr<cclient::data::Key>,bool,std::shared_ptr<cclient::data::Key>,bool,bool>(),
      "start"_a, "startInclusive"_a,"end"_a,"endInclusive"_a,"update"_a=false)
  .def(pybind11::init<std::shared_ptr<cclient::data::Key>,bool>())
  .def(pybind11::init<const std::string&,bool,const std::string&,bool,bool>(),
      "start"_a, "startInclusive"_a,"end"_a,"endInclusive"_a,"update"_a=false)
  .def("get_start_key", &cclient::data::Range::getStartKey, "Returns the start key of this range, if it exists")
  .def("get_stop_key", &cclient::data::Range::getStopKey, "Returns the stop key of this range, if it exists")
  .def("start_key_inclusive", &cclient::data::Range::getStartKeyInclusive, "Returns true if the start key is inclusive")
  .def("stop_key_inclusive", &cclient::data::Range::getStopKeyInclusive, "Returns true if the stop key is inclusive")
  .def("inifinite_start_key", &cclient::data::Range::getInfiniteStartKey, "Returns true if the start key is inclusive")
  .def("inifinite_stop_key", &cclient::data::Range::getInfiniteStopKey, "Returns true if the stop key is inclusive")
  .def("after_end_key", &cclient::data::Range::afterEndKey, "Returns true if the provided key is after this range's end key")
  .def("before_start_key", &cclient::data::Range::beforeStartKey, "Returns true if the provided key is before this range's start key")
  .def("__str__",[](const cclient::data::Range &si) {
        std::stringstream strstr;
        strstr << si;
        return strstr.str();
    })
  .def("__repr__",[](const cclient::data::Range &si) {
        std::stringstream strstr;
        strstr << si;
        return strstr.str();
    });

  pybind11::class_<cclient::data::Mutation, std::shared_ptr<cclient::data::Mutation>>(s, "Mutation")
  .def(pybind11::init<std::string>())
  .def("put", (void (cclient::data::Mutation::*)(const std::string &, const std::string &, const std::string &, int64_t, const std::string & ) ) &cclient::data::Mutation::put, "Adds a mutation")
  .def("put", (void (cclient::data::Mutation::*)(const std::string &, const std::string &, const std::string &, int64_t ) ) &cclient::data::Mutation::put, "Adds a mutation")
  .def("put", (void (cclient::data::Mutation::*)(const std::string &, const std::string &, const std::string &) ) &cclient::data::Mutation::put, "Adds a mutation")
  .def("put", (void (cclient::data::Mutation::*)(const std::string &, const std::string &) ) &cclient::data::Mutation::put, "Adds a mutation")
  .def("putDelete", (void (cclient::data::Mutation::*)(const std::string &, const std::string &, const std::string &, int64_t ) ) &cclient::data::Mutation::putDelete, "Adds a delete mutation")
  .def("putDelete", (void (cclient::data::Mutation::*)(const std::string &, const std::string &, const std::string & ) ) &cclient::data::Mutation::putDelete, "Adds a delete mutation");

  pybind11::class_<scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>>(s, "Results")
  .def("__await__", [](scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *it) -> scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>* {
            it->begin();
            return it;})
  .def("__aiter__", [](scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *it) -> scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>* {
              it->begin();
              return it;})
  .def("__iter__", [](scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *it) -> scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>* {
        it->begin();
        return it;})
  .def("__anext__", [](scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *it) -> pybind11::object {
            if (it->isEndOfRange()){
              throw stop_async_iteration();
            }
            auto ret = it->next();
            pybind11::object loop = pybind11::module::import("asyncio.events").attr("get_event_loop")();
            pybind11::object f = loop.attr("create_future")();
            f.attr("set_result")(ret);
            return f;
  })
  .def("__next__", &scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>::next);


  pybind11::enum_<ScannerOptions>(s, "ScannerOptions", pybind11::arithmetic())
       .value("HedgedReads", ScannerOptions::ENABLE_HEDGED_READS, "Enables hedged reads")
       .value("RFileScanOnly", ScannerOptions::ENABLE_RFILE_SCANNER, "Enables the RFile Scanner");

  pybind11::enum_<cclient::data::SystemPermissions>(s, "SystemPermissions", pybind11::arithmetic())
      .value("GRANT",cclient::data::SystemPermissions::GRANT, "Enables grant permission on the system")
      .value("CREATE_TABLE",cclient::data::SystemPermissions::CREATE_TABLE, "Enables create table permissions on the system")
      .value("DROP_TABLE",cclient::data::SystemPermissions::DROP_TABLE, "Enables drop table permissions on the system")
      .value("ALTER_TABLE",cclient::data::SystemPermissions::ALTER_TABLE, "Enables alter table permissions on the system")
      .value("CREATE_USER",cclient::data::SystemPermissions::CREATE_USER, "Enables create user permissions on the system")
      .value("ALTER_USER",cclient::data::SystemPermissions::ALTER_USER, "Enables alter user permissions on the system")
      .value("SYSTEM",cclient::data::SystemPermissions::SYSTEM, "Enables system permissions for the user")
      .value("CREATE_NAMESPACE",cclient::data::SystemPermissions::CREATE_NAMESPACE, "Enables create namespace permissions for the user")
      .value("DROP_NAMESPACE",cclient::data::SystemPermissions::DROP_NAMESPACE, "Enables drop namespace permissions for the user")
      .value("ALTER_NAMESPACE",cclient::data::SystemPermissions::ALTER_NAMESPACE, "Enables the alter namespace permissions for the user");

  pybind11::enum_<cclient::data::NamespacePermissions>(s, "NamespacePermissions", pybind11::arithmetic())
      .value("READ",cclient::data::NamespacePermissions::READ, "Enables read permission on the namespace") 
      .value("WRITE",cclient::data::NamespacePermissions::WRITE, "Enables write permission on the namespace") 
      .value("ALTER_NAMESPACE",cclient::data::NamespacePermissions::ALTER_NAMESPACE, "Enables the alter namespace permissions for the user")
      .value("GRANT",cclient::data::NamespacePermissions::GRANT, "Enables grant permission on the namespace")
      .value("ALTER_TABLE",cclient::data::NamespacePermissions::ALTER_TABLE, "Enables alter table permissions on the namespace")
      .value("CREATE_TABLE",cclient::data::NamespacePermissions::CREATE_TABLE, "Enables create table permissions on the namespace")
      .value("DROP_TABLE",cclient::data::NamespacePermissions::DROP_TABLE, "Enables drop table permissions on the namespace")
      .value("BULK_IMPORT",cclient::data::NamespacePermissions::BULK_IMPORT, "Enables bulk import permissions on the namespace")
      .value("DROP_NAMESPACE",cclient::data::NamespacePermissions::DROP_NAMESPACE, "Enables drop namespace permissions on the namespace");

  pybind11::enum_<cclient::data::TablePermissions>(s, "TablePermissions", pybind11::arithmetic())
      .value("READ",cclient::data::TablePermissions::READ, "Enables read permission on the table") 
      .value("WRITE",cclient::data::TablePermissions::WRITE, "Enables write permission on the table") 
      .value("GRANT",cclient::data::TablePermissions::GRANT, "Enables grant permission on the table")
      .value("ALTER_TABLE",cclient::data::TablePermissions::ALTER_TABLE, "Enables alter table permissions on the table")
      .value("DROP_TABLE",cclient::data::TablePermissions::DROP_TABLE, "Enables drop table permissions on the table")
      .value("BULK_IMPORT",cclient::data::TablePermissions::BULK_IMPORT, "Enables bulk import permissions on the table");

  pybind11::class_<scanners::BatchScanner,std::shared_ptr<scanners::BatchScanner>>(s, "BatchScanner", "Batch Scanner to be constructed via TableOperations")
  .def("getResultSet", &scanners::BatchScanner::getResultSet, pybind11::return_value_policy::reference, "Gets a result set that can be used asynchronously")
  .def("fetchColumn", &scanners::BatchScanner::fetchColumn, "Fetches the column")
  .def("setOption", &scanners::BatchScanner::setOption, "Sets the option")
  .def("__enter__",[](const std::shared_ptr<scanners::BatchScanner> &self) {
        return self;
    })
  .def("__exit__",[](std::shared_ptr<scanners::BatchScanner> &self, pybind11::object exc_type, pybind11::object exc_value, pybind11::object exc_traceback) {
       self->close();
    })
   .def("withRange", [](std::shared_ptr<scanners::BatchScanner> &self,const cclient::data::Range &r) -> std::shared_ptr<scanners::BatchScanner> {
            self->addRange( r);
            return self;})
  .def("removeOption", &scanners::BatchScanner::removeOption, "Removes an option")
  .def("addIterator", (void (scanners::BatchScanner::*)(const cclient::data::IterInfo &) ) &scanners::BatchScanner::addIterator, "Add an iterator to be run server side")
  .def("addIterator",(void (scanners::BatchScanner::*)(const cclient::data::python::PythonIterInfo &) ) &scanners::BatchScanner::addPythonIterator, "Adds a python iterator to be run server side")
  .def("close", &scanners::BatchScanner::close, "closes the scanner")
  .def("addRange",(void (scanners::BatchScanner::*)(const cclient::data::Range &) ) &scanners::BatchScanner::addRange, "Adds a range");

  pybind11::class_<cclient::data::security::Authorizations>(s, "Authorizations", "Authorizations object")
  .def(pybind11::init<>())
  .def(pybind11::init<const std::vector<std::string>&>())
  .def(pybind11::init([](const std::string &arg){
    std::vector<std::string> v;
    v.emplace_back(arg);
    return cclient::data::security::Authorizations(v);
  }))
  .def("addAuthorization",&cclient::data::security::Authorizations::addAuthorization, "Add an authorization to be used for table operations")
  .def("contains",&cclient::data::security::Authorizations::contains, "Returns true if the set contains an authorization")
  .def("get_authorizations",&cclient::data::security::Authorizations::getAuthorizations, "Returns an iterable of authorizations")
  .def("__str__",[](const cclient::data::security::Authorizations &si) {
          std::stringstream str;
          const auto vec = si.getAuthorizations();
          str << "[ ";
          std::copy(vec.begin(), vec.end()-1, 
            std::ostream_iterator<std::string>(str, ", ")); 
          str << vec.back();
          str << " ]";
          return str.str();
        
    })
  .def("__repr__",[](const cclient::data::security::Authorizations &si) {
          std::stringstream str;
          auto vec = si.getAuthorizations();
          str << "[ ";
          std::copy(vec.begin(), vec.end()-1, 
            std::ostream_iterator<std::string>(str, ", ")); 
          str << vec.back();
          str << " ]";
          return str.str();
    });

  pybind11::class_<writer::Sink<cclient::data::KeyValue>,std::shared_ptr<writer::Sink<cclient::data::KeyValue>> >(s, "BatchWriter", "Batch writer to be constructed, from TableOperations")
  .def("flush",&writer::Sink<cclient::data::KeyValue>::flush, "Flushes the batch writer. Will be called automatically by close.")
  .def("addMutation", (bool (writer::Sink<cclient::data::KeyValue>::*)(const std::shared_ptr<cclient::data::Mutation> & ) ) &writer::Sink<cclient::data::KeyValue>::addMutation, "Adds a mutation to the batch writer")
  .def("__enter__",[](const std::shared_ptr<writer::Sink<cclient::data::KeyValue>> &self) {
        return self;
    })
  .def("__exit__",[](std::shared_ptr<writer::Sink<cclient::data::KeyValue>> &self, pybind11::object exc_type, pybind11::object exc_value, pybind11::object exc_traceback) {
       self->close();
    })
  .def("close",&writer::Sink<cclient::data::KeyValue>::close, "Closes the batch writer")
  .def("size",&writer::Sink<cclient::data::KeyValue>::size, "Returns the current size to be written to Accumulo");

  pybind11::class_<cclient::data::RFile>(s, "RFile", "RFile, which is an internal data structure for storing data within Accumulo")
  .def("seek",&cclient::data::RFile::relocate, "Seek to data")
  .def("hasNext",&cclient::data::RFile::hasNext, "Returns true of further keys exist, false otherwise")
  .def("getTop",&cclient::data::RFile::getTop, "Returns the top key/value")
  .def("close",&cclient::data::RFile::close, "Closes the RFile")
  .def("next",&cclient::data::RFile::next, "Queues the next key/value pair ");


pybind11::class_<cclient::data::streams::KeyValueIterator, std::shared_ptr<cclient::data::streams::KeyValueIterator>>(s, "KeyValueIterator", "Iterable object within Accumulo")
  .def(pybind11::init<>(), "Constructor for KeyValueIterator")
  .def("seek",&cclient::data::streams::KeyValueIterator::relocate, "Seeks to a key within the iterator")
  .def("hasNext",&cclient::data::streams::KeyValueIterator::hasNext, "Returns true of further keys exist, false otherwise")
  .def("getTopKey",&cclient::data::streams::KeyValueIterator::getTopKey, "Returns the top key")
  .def("getTopValue",&cclient::data::streams::KeyValueIterator::getTopValue, "Returns the top value")
  .def("next",&cclient::data::streams::KeyValueIterator::next, "Queues the next key/value to be returned via getTopKey and getTopValue");

  pybind11::class_<cclient::data::SequentialRFile, cclient::data::streams::KeyValueIterator, std::shared_ptr<cclient::data::SequentialRFile>>(s, "SequentialRFile", "Specializd RFile, which is an internal data structure for storing data within Accumulo, to be used when much of the data is similar")
  .def("seek",&cclient::data::SequentialRFile::relocate, "Seeks to the next key and value within the RFile")
  .def("hasNext",&cclient::data::SequentialRFile::hasNext,"Returns true of further keys exist, false otherwise")
  .def("getTop",&cclient::data::SequentialRFile::getTop, "Returns the top key/value")
  .def("addLocalityGroup",&cclient::data::SequentialRFile::addLocalityGroup, "Adds a locality group to an RFile when writing")
  .def("append",(bool (cclient::data::SequentialRFile::*)(std::shared_ptr<cclient::data::KeyValue> ) )&cclient::data::SequentialRFile::append, "Appends a key and value pair to the RFile")
  .def("close",&cclient::data::SequentialRFile::close, "Closes the RFile")
  .def("next",&cclient::data::SequentialRFile::next, "Queues the next key/value pair");

  pybind11::class_<cclient::data::hdfs::HdfsDirEnt>(s, "HdfsDirEnt", "HDFS directory entry object. ")
      .def("getName",&cclient::data::hdfs::HdfsDirEnt::getName, "Gets the name of the directory entry")
      .def("getOwner",&cclient::data::hdfs::HdfsDirEnt::getOwner, "Gets the owner of the directory entry")
      .def("getGroup",&cclient::data::hdfs::HdfsDirEnt::getGroup, "Gets the group of the directory entry")
      .def("getSize",&cclient::data::hdfs::HdfsDirEnt::getSize, "Returns the size of the directory entry")
      .def("__str__",[](const cclient::data::hdfs::HdfsDirEnt self) {
        return self.getOwner() + " " + self.getGroup() + " " + std::to_string(self.getSize()) + " " + self.getName();
    });

  pybind11::class_<cclient::data::streams::HdfsOutputStream, std::shared_ptr<cclient::data::streams::HdfsOutputStream>>(s, "HdfsOutputStream", "HDFS Output Stream")
    .def("writeShort",&cclient::data::streams::HdfsOutputStream::writeShort, "Writes a two byte integer")
    .def("writeLong",&cclient::data::streams::HdfsOutputStream::writeLong, "Writes an eight byte integer")
    .def("write", (uint64_t (cclient::data::streams::HdfsOutputStream::*)(const char *, long ) )&cclient::data::streams::HdfsOutputStream::write, "Writes an eight byte integer")
    .def("writeString", &cclient::data::streams::HdfsOutputStream::writeString, "Writes a string")
    .def("writeInt",&cclient::data::streams::HdfsOutputStream::writeInt, "Writes a four byte integer");

    pybind11::class_<cclient::data::streams::HdfsInputStream, std::shared_ptr<cclient::data::streams::HdfsInputStream>>(s, "HdfsInputStream", "HDFS Input Stream")
    .def("readShort",&cclient::data::streams::HdfsInputStream::readShort, "Reads a two byte integer")
    .def("readLong",&cclient::data::streams::HdfsInputStream::readLong, "Reads an eight byte integer")
    .def("readBytes", (uint64_t (cclient::data::streams::HdfsInputStream::*)(char *, size_t ) )&cclient::data::streams::HdfsInputStream::readBytes, "Reads a character sequence from the file on HDFS")
    .def("readString", &cclient::data::streams::HdfsInputStream::readString, "Reads a string")
    .def("readInt",&cclient::data::streams::HdfsInputStream::readInt, "Reads a four byte integer");
    

  pybind11::class_<cclient::data::hdfs::HdfsLink, std::shared_ptr<cclient::data::hdfs::HdfsLink>>(s, "Hdfs", "HDFS refernce object")
    .def(pybind11::init<std::string,int>(), "Constructs an HDFS object with the namenode host name and the namenode port")
    .def("write",&cclient::data::hdfs::HdfsLink::write, "Opens a write stream to an HDFS file, creating or updating it")
    .def("read",&cclient::data::hdfs::HdfsLink::read, "Opens a read stream to an HDFS file, creating or updating it")
    .def("remove",&cclient::data::hdfs::HdfsLink::remove, "Removes a file or directory, the boolean flag, if set to true, deletes recusively")
    .def("rename",&cclient::data::hdfs::HdfsLink::rename, "Renames a path")
    .def("move",&cclient::data::hdfs::HdfsLink::move, "Moves a file within a NN instance.")
    .def("chown",&cclient::data::hdfs::HdfsLink::chown, "Chowns the provided path")
    .def("mkdir",&cclient::data::hdfs::HdfsLink::mkdir, "Creates a directory on HDFS. Should be a relative path")
    .def("list",&cclient::data::hdfs::HdfsLink::list, "Lists HDFS directory, returns a list of HdfsDirEnt objects");

    pybind11::class_<cclient::data::streams::StreamRelocation>(s, "StreamRelocation");

  pybind11::class_<cclient::data::streams::StreamSeekable, cclient::data::streams::StreamRelocation>(s, "Seekable")
    .def(pybind11::init<cclient::data::Range&>())
    .def(pybind11::init<cclient::data::Range&,std::vector<std::string> &,bool>())
    .def("getRange",&cclient::data::streams::StreamSeekable::getRange, "Gets this seekable range")
    .def("getColumnFamilies",&cclient::data::streams::StreamSeekable::getColumnFamilies, "Gets the column families for this seekable")
    .def("isInclusive",&cclient::data::streams::StreamSeekable::isInclusive, "Returns true if the column families are inclusive.");

  pybind11::class_<cclient::data::RFileOperations>(s, "RFileOperations", "RFile Operations")
    .def("randomSeek",&cclient::data::RFileOperations::open, "Opens a single RFile to read, best used for random seeks")
    .def("openForWrite",&cclient::data::RFileOperations::openForWrite, "Opens an RFile to write")
    .def("sequentialRead",&cclient::data::RFileOperations::openSequential, "Opens an RFile to read sequentially")
    .def("openManySequential",&cclient::data::RFileOperations::openManySequential, "Opens many RFiles sequentially.");


pybind11::class_<interconnect::PythonSecurityOperations>(s, "SecurityOperations", "Basic security operations")
  .def("create_user",&interconnect::PythonSecurityOperations::createUser, "Creates a user")
  .def("change_password",&interconnect::PythonSecurityOperations::changeUserPassword, "Changes the user password")
  .def("remove_user",&interconnect::PythonSecurityOperations::dropUser, "Removes the user")
  .def("get_auths",&interconnect::PythonSecurityOperations::getAuths, "Returns the user's authorizations")
  .def("has_system_permission",&interconnect::PythonSecurityOperations::hasSystemPermission, "Returns true if the user has the system permission")
  .def("has_table_permission",&interconnect::PythonSecurityOperations::hasTablePermission, "Has table permission")
  .def("has_namespace_permission",&interconnect::PythonSecurityOperations::hasNamespacePermission, "Has namespace permission")  
  .def("grant_system_permission",&interconnect::PythonSecurityOperations::grantSystemPermission, "Grants a system permission")
  .def("revoke_system_permission",&interconnect::PythonSecurityOperations::revokeSystemPermission, "Revokes a system permission")
  .def("grant_table_permission",&interconnect::PythonSecurityOperations::grantTablePermission, "Grants a table permission")
  .def("revoke_table_permission",&interconnect::PythonSecurityOperations::revokeTablePermission, "Revokes a table permission")
  .def("grant_namespace_permission",&interconnect::PythonSecurityOperations::grantNamespacePermission, "Grants a namespace permission")
  .def("revoke_namespace_permission",&interconnect::PythonSecurityOperations::revokeNamespacePermission, "Revokes a namespace permission")
  .def("grantAuthorizations",&interconnect::PythonSecurityOperations::grantAuthorizations, "Get user authorizations");



  pybind11::register_exception<apache::thrift::TApplicationException>(s, "TApplicationException");
  static pybind11::exception<cclient::exceptions::ClientException> exc(s, "ClientException");
    pybind11::register_exception_translator([](std::exception_ptr p) {
        try {
            if (p) std::rethrow_exception(p);
        } catch (const cclient::exceptions::ClientException &e) {
            exc(e.what());
        }
    });
}
