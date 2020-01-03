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

  static Interpreter* getInterpreter();

  py::scoped_interpreter guard_;
  py::gil_scoped_release gil_release_;
};

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
  void call(const std::string &fn_name, Args &&...args);

  template<typename ... Args>
  bool callOptional(const std::string &fn_name, Args &&...args);

  template<typename T, typename ... Args>
  T callWithReturn(const std::string &fn_name, Args &&...args);

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

  std::shared_ptr<cclient::data::KeyValue> callNext(cclient::jni::DSLIterator *iter) {
    return callWithReturn<std::shared_ptr<cclient::data::KeyValue>>("onNext", iter);
  }

  bool callSeek(cclient::jni::DSLIterator *iter, const std::shared_ptr<cclient::data::Range> &range) {
    return callOptional("seek", iter, range);
  }

  bool callHasTop() {
    return callWithReturn<bool>("hasTop");
  }

  std::shared_ptr<cclient::data::Key> callGetTopKey() {
    return callWithReturn<std::shared_ptr<cclient::data::Key>>("getTopKey");
  }

  std::shared_ptr<cclient::data::Value> callGetTopValue() {
    return callWithReturn<std::shared_ptr<cclient::data::Value>>("getTopValue");
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
  } catch (pybind11::error_already_set &err) {
    err.restore();
    throw JavaException("Python Syntax error");
  } catch (const std::exception &e) {
    throw JavaException("Error called running " + fn_name);
  } catch (...) {
    throw JavaException("Error called running " + fn_name);
  }

}

template<typename ... Args>
bool IteratorPythonExecutor::callOptional(const std::string &fn_name, Args &&...args) {
  py::gil_scoped_acquire gil { };
  if (!(*bindings_).contains(fn_name.c_str()))
    return false;  // safe
  try {
    (*bindings_)[fn_name.c_str()](convert(args)...);
    return true;
  } catch (pybind11::error_already_set &err) {
    err.restore();
    throw JavaException("Python Syntax error");
  } catch (const std::exception &e) {
    throw JavaException(e.what());
  } catch (...) {
    throw JavaException("Error called running " + fn_name);
  }
}

template<typename T, typename ... Args>
T IteratorPythonExecutor::callWithReturn(const std::string &fn_name, Args &&...args) {
  py::gil_scoped_acquire gil { };
  try {

    std::cout << " call " << fn_name << std::endl;
    if ((*bindings_).contains(fn_name.c_str())) {
      pybind11::object result = (*bindings_)[fn_name.c_str()](convert(args)...);
      std::cout << " called " << fn_name << std::endl;
      return result.cast<T>();
    }
    throw JavaException("No defined function for " + fn_name);
  } catch (pybind11::error_already_set &err) {
    err.restore();
    throw JavaException("Python Syntax error");
  } catch (const std::exception &e) {
    throw JavaException(e.what());
  } catch (...) {
    throw JavaException("Error called running " + fn_name);
  }
}

} /* namespace python */
} /* namespace jni */
} /* namespace cclient */

#endif //_ITERATOR_EXECUTOR_H_
