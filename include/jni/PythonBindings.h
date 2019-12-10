 /**
 * @file ScriptException.h
 * ScriptException class declaration
 *
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#include <pybind11/embed.h>
#include <pybind11/pybind11.h>
#include <pybind11/stl.h>
#include <pybind11/functional.h>


#include "DSLIterator.h"
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
#include "data/constructs/compressor/compressor.h"
#include "data/constructs/compressor/zlibCompressor.h"
#include "../include/logging/Logger.h"
#include "../include/logging/LoggerConfiguration.h"
#include "Iterators.h"
#include "PythonIterator.h"

PYBIND11_DECLARE_HOLDER_TYPE(T, std::shared_ptr<T>);

PYBIND11_EMBEDDED_MODULE(sharkbite_native_iterator, m) { // NOLINT
  namespace py = pybind11;


  pybind11::class_<cclient::data::Value, std::shared_ptr<cclient::data::Value>>(m, "Value")
     .def(pybind11::init<>())
     .def("get", &cclient::data::Value::getValueAsString, "Returns the value as a UTF-8 string");

   pybind11::class_<cclient::data::Key, std::shared_ptr<cclient::data::Key>>(m, "Key")
     .def(pybind11::init<>())
     .def("setRow",(void (cclient::data::Key::*)(const std::string &) )  &cclient::data::Key::setRow, "Sets the row")
     .def("setColumnFamily",(void (cclient::data::Key::*)(const std::string &) )  &cclient::data::Key::setColFamily, "Sets the column family")
     .def("setColumnQualifier",(void (cclient::data::Key::*)(const std::string &) )  &cclient::data::Key::setColQualifier, "Sets the column qualifier")
     .def("getRow", &cclient::data::Key::getRowStr, "Gets the row")
     .def("getColumnFamily", &cclient::data::Key::getColFamilyStr, "Gets the Column Family")
     .def("getColumnVisibility", &cclient::data::Key::getColVisibilityStr, "Gets the Column Visibility")
     .def("getTimestamp", &cclient::data::Key::getTimeStamp, "Gets the key timestamp")
     .def("getColumnQualifier", &cclient::data::Key::getColQualifierStr, "Gets the Column Qualifier");

   pybind11::class_<cclient::data::Range,std::shared_ptr<cclient::data::Range>>(m, "Range")
           .def(pybind11::init<const std::string&>())
           .def(pybind11::init<std::shared_ptr<cclient::data::Key>,bool,std::shared_ptr<cclient::data::Key>,bool>());


   pybind11::class_<cclient::jni::python::PythonIterator>(m, "PythonIterator")
      .def(pybind11::init<>())
      .def("seek",&cclient::jni::python::PythonIterator::seek)
      .def("next", &cclient::jni::python::PythonIterator::next)
      .def("hasTop", &cclient::jni::python::PythonIterator::iteratorHasTop)
      .def("getTopKey", &cclient::jni::python::PythonIterator::getIteratorTopKey)
      .def("getTopValue", &cclient::jni::python::PythonIterator::getIteratorTopValue);


}
