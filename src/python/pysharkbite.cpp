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

#include "data/constructs/PythonIterInfo.h"
#include "data/constructs/IterInfo.h"
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
//logging::LoggerConfiguration::getConfiguration().enableLogging(logging::LOG_LEVEL::trace);
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

  pybind11::class_<interconnect::MasterConnect>(s, "AccumuloConnector", "Accumulo connector")
  .def(pybind11::init<cclient::data::security::AuthInfo&, std::shared_ptr<cclient::data::Instance>>())
  .def("securityOps",&interconnect::MasterConnect::securityOps, "Return the security operations object")
  .def("tableOps",&interconnect::MasterConnect::tableOps, "Return the table operations object");

  pybind11::class_<interconnect::AccumuloTableOperations>(s, "AccumuloTableOperations", "Accumulo table operations. Should be accessed through 'AccumuloConnector'")
  .def("remove",&interconnect::AccumuloTableOperations::remove, "remove the table")
  .def("exists",&interconnect::AccumuloTableOperations::exists, "Returns true if the table exists")
  .def("import",&interconnect::AccumuloTableOperations::import, "import data into this directory")
  .def("flush",&interconnect::AccumuloTableOperations::flush, "flush the table")
  .def("compact", &interconnect::AccumuloTableOperations::compact, "compact the table")
  .def("setProperty", &interconnect::AccumuloTableOperations::setProperty, "Set table property")
  .def("removeProperty", &interconnect::AccumuloTableOperations::removeProperty, "Remove the table property")
  .def("addSplits", &interconnect::AccumuloTableOperations::addSplits, "Add splits for a table")
  .def("addConstraint", &interconnect::AccumuloTableOperations::addConstraint, "Add table constraint")
  .def("createScanner", &interconnect::AccumuloTableOperations::createScanner, "Create scanner")
  .def("createWriter", &interconnect::AccumuloTableOperations::createWriter, "Create writer for table")
  .def("create",&interconnect::AccumuloTableOperations::create, "Create the table");

  pybind11::class_<interconnect::SecurityOperations>(s, "SecurityOperations", "Basic security operations")
  .def("grantAuthorizations",&interconnect::SecurityOperations::grantAuthorizations, "Get user authorizations");

  pybind11::class_<cclient::data::Value, std::shared_ptr<cclient::data::Value>>(s, "Value", "Accumulo value")
  .def(pybind11::init<>())
  .def("get", &cclient::data::Value::getValueAsString, "Returns the value as a UTF-8 string")
  .def("get_bytes",
      [](cclient::data::Value &self) {
          return pybind11::bytes(self.getValueAsString());  // Return the data without transcoding
      }, "Returns the bytes as python bytes"
  )
  .def("__str__",[](const std::shared_ptr<cclient::data::Value> &self) {
        return self->getValueAsString();
    });

  pybind11::class_<cclient::data::Key, std::shared_ptr<cclient::data::Key>>(s, "Key", "Accumulo Key")
  .def(pybind11::init<>())
  .def(pybind11::init<const char *,const char *, const char *, const char *, int64_t>(),
      "row"_a, "columnfamily"_a=nullptr,"columnqualifier"_a=nullptr,"columnvisibility"_a=nullptr,"timestamp"_a=9223372036854775807L)
  .def("setRow",(void (cclient::data::Key::*)(const std::string &) ) &cclient::data::Key::setRow, "Sets the row")
  .def("__str__",[](const std::shared_ptr<cclient::data::Key> &si) {
        return si->toString();
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
      "start"_a, "startInclusive"_a,"end"_a,"endInclusive"_a,"update"_a=false);

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

  pybind11::class_<scanners::BatchScanner>(s, "BatchScanner", "Batch Scanner to be constructed via TableOperations")
  .def("getResultSet", &scanners::BatchScanner::getResultSet, pybind11::return_value_policy::reference, "Gets a result set that can be used asynchronously")
  .def("fetchColumn", &scanners::BatchScanner::fetchColumn, "Fetches the column")
  .def("setOption", &scanners::BatchScanner::setOption, "Sets the option")
  .def("removeOption", &scanners::BatchScanner::removeOption, "Removes an option")
  .def("addIterator", (void (scanners::BatchScanner::*)(const cclient::data::IterInfo &) ) &scanners::BatchScanner::addIterator, "Add an iterator to be run server side")
  .def("addIterator",(void (scanners::BatchScanner::*)(const cclient::data::python::PythonIterInfo &) ) &scanners::BatchScanner::addPythonIterator, "Adds a python iterator to be run server side")
  .def("close", &scanners::BatchScanner::close, "closes the scanner")
  .def("addRange",(void (scanners::BatchScanner::*)(const cclient::data::Range &) ) &scanners::BatchScanner::addRange, "Adds a range");

  pybind11::class_<cclient::data::security::Authorizations>(s, "Authorizations", "Authorizations object")
  .def(pybind11::init<>())
  .def("addAuthorization",&cclient::data::security::Authorizations::addAuthorization, "Add an authorization to be used for table operations");

  pybind11::class_<writer::Sink<cclient::data::KeyValue>>(s, "BatchWriter", "Batch writer to be constructed, from TableOperations")
  .def("flush",&writer::Sink<cclient::data::KeyValue>::flush, "Flushes the batch writer. Will be called automatically by close.")
  .def("addMutation", (bool (writer::Sink<cclient::data::KeyValue>::*)(const std::shared_ptr<cclient::data::Mutation> & ) ) &writer::Sink<cclient::data::KeyValue>::addMutation, "Adds a mutation to the batch writer")
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

}
