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

#ifndef _ITERATOR_EXECUTOR_H_
#define _ITERATOR_EXECUTOR_H_

#include <mutex>
#include <pybind11/embed.h>
#include <pybind11/pybind11.h>
#include <pybind11/stl.h>
#include <pybind11/functional.h>
#include "JavaException.h"
#include "Iterators.h"

namespace cclient {
namespace jni {
namespace python {

namespace py = pybind11;

struct Interpreter {

  Interpreter()
      :
      guard_(false) {
  }

  ~Interpreter() {
  }

  Interpreter(const Interpreter &other) = delete;

  py::scoped_interpreter guard_;
  py::gil_scoped_release gil_release_;
};

static Interpreter* getInterpreter();

class IteratorPythonExecutor {
 public:
  IteratorPythonExecutor();
  virtual ~IteratorPythonExecutor() {
    py::gil_scoped_acquire gil { };
    (*bindings_).dec_ref();
    (*bindings_).release();
  }

  /**
   * Initializes the python interpreter.
   */
  static void initialize();

  /**
   * Evaluates a python expression.
   *
   * @param script
   */
  void eval(const std::string &script);

  /**
   * Evaluates the contents of the given python file name.
   *
   * @param file_name
   */
  void evalFile(const std::string &file_name);

  /**
   * Calls the given function, forwarding arbitrary provided parameters.
   *
   * @return
   */
  template<typename ... Args>
  void call(const std::string &fn_name, Args &&...args) {
    py::gil_scoped_acquire gil { };
    try {
      if ((*bindings_).contains(fn_name.c_str()))
        (*bindings_)[fn_name.c_str()](convert(args)...);
    } catch (const std::exception &e) {
      throw JavaException(e.what());
    }
  }

  template<typename T,typename ... Args>
    T callWithReturn(const std::string &fn_name, Args &&...args) {
      py::gil_scoped_acquire gil { };
      try {
        if ((*bindings_).contains(fn_name.c_str())){
          pybind11::object result = (*bindings_)[fn_name.c_str()](convert(args)...);
          return result.cast<T>();
        }
        throw JavaException("No defined function");
      } catch (const std::exception &e) {
        throw JavaException(e.what());
      }
    }

  /**
   * Calls the given function, forwarding arbitrary provided parameters.
   *
   * @return
   */
  template<typename ... Args>
  void callRequiredFunction(const std::string &fn_name, Args &&...args) {
    py::gil_scoped_acquire gil { };
    if (!(*bindings_).contains(fn_name.c_str()))
      throw std::runtime_error("Required Function" + fn_name + " is not found within Python bindings");
    (*bindings_)[fn_name.c_str()](convert(args)...);
  }

  void callNext(cclient::jni::DSLIterator *iter) {
    call("next", iter);
  }

  std::shared_ptr<cclient::data::Key> callGetTopKey(cclient::jni::DSLIterator *iter) {
    return callWithReturn<std::shared_ptr<cclient::data::Key>>("getTopKey", iter);
  }

  std::shared_ptr<cclient::data::Value> callGetTopValue(cclient::jni::DSLIterator *iter) {
    return callWithReturn<std::shared_ptr<cclient::data::Value>>("getTopValue", iter);
  }

  /**
   * Binds an object into the scope of the python interpreter.
   * @tparam T
   * @param name
   * @param value
   */
  template<typename T>
  void bind(const std::string &name, const T &value) {
    py::gil_scoped_acquire gil { };
    (*bindings_)[name.c_str()] = convert(value);
  }

  template<typename T>
  py::object convert(const T &value) {
    py::gil_scoped_acquire gil { };
    return py::cast(value);
  }

 private:
  std::unique_ptr<py::dict> bindings_;
};

} /* namespace python */
} /* namespace jni */
} /* namespace cclient */

#endif //_ITERATOR_EXECUTOR_H_
