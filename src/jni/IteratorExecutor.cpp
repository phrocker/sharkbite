/**
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

#include <memory>
#include <string>
#include <pybind11/pybind11.h>
#include <pybind11/stl.h>
#include <pybind11/functional.h>
#include "jni/PythonBindings.h"
#include "jni/IteratorExecutor.h"

namespace cclient {
namespace jni {
namespace python {

Interpreter* getInterpreter() {
  static Interpreter interpreter;
  return &interpreter;
}

IteratorPythonExecutor::IteratorPythonExecutor() {
  auto intepreter = getInterpreter();
  py::gil_scoped_acquire gil { };
  py::module::import("sharkbite_native_iterator");
  bindings_.reset(new py::dict());
  (*bindings_) = py::globals().attr("copy")();
}

void IteratorPythonExecutor::eval(const std::string &script) {
  py::gil_scoped_acquire gil { };

  if (script[0] == '\n') {
    py::eval<py::eval_statements>(py::module::import("textwrap").attr("dedent")(script), *bindings_, *bindings_);
  } else {
    py::eval<py::eval_statements>(script, *bindings_, *bindings_);
  }
}

void IteratorPythonExecutor::initialize() {
  auto intepreter = getInterpreter();
}


/**
 * Calls the given function, forwarding arbitrary provided parameters.
 *
 * @return
 */
template<typename ... Args>
void IteratorPythonExecutor::call(const std::string &fn_name, Args &&...args) {
  py::gil_scoped_acquire gil { };
  try {
    if ((*bindings_).contains(fn_name.c_str()))
      (*bindings_)[fn_name.c_str()](convert(args)...);
  } catch (const std::exception &e) {
    throw JavaException(e.what());
  }

}

template<typename T, typename ... Args>
T IteratorPythonExecutor::callWithReturn(const std::string &fn_name, Args &&...args) {
  py::gil_scoped_acquire gil { };
  try {
    if ((*bindings_).contains(fn_name.c_str())) {
      pybind11::object result = (*bindings_)[fn_name.c_str()](convert(args)...);
//      if (std::is_pointer<T>() && result.is_none())
  //      return nullptr;
      return result.cast<T>();
    }
    throw JavaException("No defined function");
  } catch (const std::exception &e) {
    throw JavaException(e.what());
  }
}

} /* namespace python */
} /* namespace jni */
} /* namespace cclient */

