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

#include "data/constructs/KeyValue.h"
#include "data/constructs/Mutation.h"
#include "data/constructs/security/Authorizations.h"
#include "scanner/constructs/Results.h"
#include "scanner/impl/Scanner.h"
#include "scanner/constructs/Results.h"
#include "writer/impl/SinkImpl.h"
#include "data/constructs/client/zookeeperinstance.h"
#include "interconnect/Master.h"
#include "interconnect/tableOps/TableOperations.h"

#include "data/constructs/rfile/RFile.h"
#include "data/constructs/compressor/compressor.h"
#include "data/constructs/compressor/zlibCompressor.h"
#include "../include/logging/Logger.h"
#include "../include/logging/LoggerConfiguration.h"


PYBIND11_DECLARE_HOLDER_TYPE(T, std::shared_ptr<T>);

PYBIND11_MODULE(pysharkbite, s){
  s.doc() = "Accumulo connector plugin";

  pybind11::class_<cclient::impl::Configuration, std::shared_ptr<cclient::impl::Configuration>>(s, "Configuration")
     .def(pybind11::init<>())
     .def("set", &cclient::impl::Configuration::set)
     .def("get", (std::string (cclient::impl::Configuration::*)(const std::string &) const) &cclient::impl::Configuration::get, "Get the configuration option")
     .def("get", (std::string (cclient::impl::Configuration::*)(const std::string &, const std::string &) const) &cclient::impl::Configuration::get, "Get the configuration option with default value")
     .def("getLong", (uint32_t (cclient::impl::Configuration::*)(const std::string &, uint32_t) const) &cclient::impl::Configuration::getLong, "Get the integer value of a configuration item");

  pybind11::class_<cclient::data::Instance>(s, "Instance");
//logging::LoggerConfiguration::getConfiguration().enableLogging(logging::LOG_LEVEL::trace);
  pybind11::class_<logging::LoggerConfiguration>(s, "LoggingConfiguration")
    .def_static("enableLogging",&logging::LoggerConfiguration::enableLogger);

  pybind11::class_<cclient::data::zookeeper::ZookeeperInstance, cclient::data::Instance>(s, "ZookeeperInstance")
	  .def(pybind11::init<std::string, std::string,uint32_t, const std::shared_ptr<cclient::impl::Configuration>&>())
	  .def("getInstanceName", &cclient::data::zookeeper::ZookeeperInstance::getInstanceName)
	  .def("getInstanceId", &cclient::data::zookeeper::ZookeeperInstance::getInstanceId);


  pybind11::class_<cclient::data::security::AuthInfo>(s, "AuthInfo")
      .def(pybind11::init<std::string, std::string,std::string>())
      .def("getUserName",&cclient::data::security::AuthInfo::getUserName)
      .def("getPassword", &cclient::data::security::AuthInfo::getPassword)
      .def("getInstanceId", &cclient::data::security::AuthInfo::getInstanceId);

  pybind11::class_<interconnect::MasterConnect>(s, "AccumuloConnector")
      .def(pybind11::init<cclient::data::security::AuthInfo&, cclient::data::Instance*>())
      .def("tableOps",&interconnect::MasterConnect::tableOps);

  pybind11::class_<interconnect::AccumuloTableOperations>(s, "AccumuloTableOperations")
      .def("remove",&interconnect::AccumuloTableOperations::remove)
      .def("exists",&interconnect::AccumuloTableOperations::exists)
      .def("import",&interconnect::AccumuloTableOperations::import)
      .def("flush",&interconnect::AccumuloTableOperations::flush)
      .def("compact", &interconnect::AccumuloTableOperations::compact)
	  .def("setProperty", &interconnect::AccumuloTableOperations::setProperty)
      .def("removeProperty", &interconnect::AccumuloTableOperations::removeProperty)
      .def("addSplits", &interconnect::AccumuloTableOperations::addSplits)
      .def("addConstraint", &interconnect::AccumuloTableOperations::addConstraint)
      .def("createScanner", &interconnect::AccumuloTableOperations::createScanner)
      .def("createWriter", &interconnect::AccumuloTableOperations::createWriter)
      .def("create",&interconnect::AccumuloTableOperations::create);



  pybind11::class_<cclient::data::Value, std::shared_ptr<cclient::data::Value>>(s, "Value")
		.def(pybind11::init<>())
		.def("get", &cclient::data::Value::getValueAsString, "Returns the value as a UTF-8 string");

  pybind11::class_<cclient::data::Key, std::shared_ptr<cclient::data::Key>>(s, "Key")
		.def(pybind11::init<>())
		.def("setRow",(void (cclient::data::Key::*)(const std::string &) )  &cclient::data::Key::setRow, "Sets the row")
		.def("setColumnFamily",(void (cclient::data::Key::*)(const std::string &) )  &cclient::data::Key::setColFamily, "Sets the column fmaily")
		.def("setColumnQualifier",(void (cclient::data::Key::*)(const std::string &) )  &cclient::data::Key::setColQualifier, "Sets the column qualifier")
		.def("getRow", &cclient::data::Key::getRowStr, "Gets the row")
		.def("getColumnFamily", &cclient::data::Key::getColFamilyStr, "Gets the Column Family")
		.def("getColumnVisibility", &cclient::data::Key::getColVisibilityStr, "Gets the Column Visibility")
		.def("getTimestamp", &cclient::data::Key::getTimeStamp, "Gets the key timestamp")
		.def("getColumnQualifier", &cclient::data::Key::getColQualifierStr, "Gets the Column Qualifier");


  pybind11::class_<cclient::data::KeyValue, std::shared_ptr<cclient::data::KeyValue>>(s, "KeyValue")
		.def(pybind11::init<>())
		.def("getKey", &cclient::data::KeyValue::getKey, "Gets the Key")
		.def("getValue", &cclient::data::KeyValue::getValue, "Gets the Value");


  pybind11::class_<cclient::data::Range>(s, "Range")
        .def(pybind11::init<std::shared_ptr<cclient::data::Key>,bool,std::shared_ptr<cclient::data::Key>,bool>());

  pybind11::class_<cclient::data::Mutation, std::shared_ptr<cclient::data::Mutation>>(s, "Mutation")
        .def(pybind11::init<std::string>())
        .def("put", (void (cclient::data::Mutation::*)(const std::string &, const std::string &, const std::string &, unsigned long, const std::string & ) ) &cclient::data::Mutation::put, "Adds a mutation");


  pybind11::class_<scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>>(s, "Results")
		.def("__iter__", [](scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *it) -> scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>* {
	  	  	  it->begin();
	  	  	  return it; })
		.def("__next__", &scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>::next);

  pybind11::class_<scanners::BatchScanner>(s, "BatchScanner")
		.def("getResultSet",  &scanners::BatchScanner::getResultSet, pybind11::return_value_policy::reference)
		.def("fetchColumn", &scanners::BatchScanner::fetchColumn)
    .def("addRange",(void (scanners::BatchScanner::*)(const cclient::data::Range &) ) &scanners::BatchScanner::addRange, "Adds a range");

  pybind11::class_<cclient::data::security::Authorizations>(s, "Authorizations")
        .def(pybind11::init<>())
        .def("addAuthorization",&cclient::data::security::Authorizations::addAuthorization);




  pybind11::class_<writer::Sink<cclient::data::KeyValue>>(s, "BatchWriter")
        .def("flush",&writer::Sink<cclient::data::KeyValue>::flush)
        .def("addMutation", (bool (writer::Sink<cclient::data::KeyValue>::*)(const std::shared_ptr<cclient::data::Mutation> & ) ) &writer::Sink<cclient::data::KeyValue>::addMutation)
        .def("close",&writer::Sink<cclient::data::KeyValue>::close)
        .def("size",&writer::Sink<cclient::data::KeyValue>::size);
}
