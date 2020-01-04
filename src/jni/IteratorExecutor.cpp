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

namespace py = pybind11;

Interpreter* Interpreter::getInterpreter() {
  static Interpreter interpreter;
  return &interpreter;
}

IteratorPythonExecutor::IteratorPythonExecutor() {
  auto intepreter = Interpreter::getInterpreter();
  py::gil_scoped_acquire gil { };
  py::module::import("sharkbite_iterator");
  bindings_.reset(new py::dict());
  (*bindings_) = py::globals().attr("copy")();
}

void IteratorPythonExecutor::eval(const std::string &script) {
  py::gil_scoped_acquire gil { };
  try {
    if (script[0] == '\n') {
      py::eval<py::eval_statements>(py::module::import("textwrap").attr("dedent")(script), *bindings_, *bindings_);
    } else {
      //py::eval<py::eval_statements>(script, *bindings_, *bindings_);
      std:cout << "exec" << std::endl;
      py::exec(script, *bindings_, *bindings_);
    }

  } catch (pybind11::error_already_set &err) {
    err.restore();
    throw JavaException("Python Syntax error");
  }
}

void IteratorPythonExecutor::initialize() {
  auto intepreter = Interpreter::getInterpreter();
}

} /* namespace python */
} /* namespace jni */
} /* namespace cclient */

