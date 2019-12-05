/**
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

#ifndef _PYTHONITERATOR_
#define _PYTHONITERATOR_

#include <pybind11/embed.h>

#include "Iterators.h"
#include "IteratorExecutor.h"

namespace cclient {
namespace jni {
namespace python {

class PythonIterator : public cclient::jni::DSLIterator {

  std::shared_ptr<cclient::data::Key> topKey;
  std::shared_ptr<cclient::data::Value> topValue;

 public:

  PythonIterator() {
  }

  ~PythonIterator() {
  }

  virtual void setDSL(const std::string &dsl) override {
    iter.eval(dsl);
  }

  virtual void callNext() override {
    iter.callNext(this);
  }

  virtual void callGetTopKey() override {
    topKey = iter.callGetTopKey(this);
  }

  virtual void callgetTopValue() override {
    topValue = iter.callGetTopValue(this);
  }

  virtual std::shared_ptr<cclient::data::Key> getTopKey() override {
    return topKey;
  }
  virtual std::shared_ptr<cclient::data::Value> getTopValue() override {
   return topValue;
  }

  virtual bool hasTop() override {
    return (nullptr != topKey);
  }

 private:
  IteratorPythonExecutor iter;

};

} /* namespace python */
} /* namespace jni */
} /* namespace cclient */

#endif //_PYTHONITERATOR_
