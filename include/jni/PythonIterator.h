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

#include "data/constructs/KeyValue.h"
#include "Iterators.h"
#include "IteratorExecutor.h"

namespace cclient {
namespace jni {
namespace python {

class PythonIterator : public cclient::jni::DSLIterator {

  std::shared_ptr<cclient::data::KeyValue> topKeyValue;
  std::shared_ptr<cclient::data::Key> topKey;
  std::shared_ptr<cclient::data::Value> topValue;

  bool calledNext;

 public:

  PythonIterator()
      :
      calledNext(false) {
  }

  ~PythonIterator() {
  }

  virtual void setDSL(const std::string &dsl) override;
  virtual void callNext() override;

  virtual void callSeek(const std::shared_ptr<cclient::data::Range> &range) override;

  virtual bool callHasTop() override {
    return (nullptr != topKey);
  }

  virtual void callGetTopKey() override {
    // nothing to do
  }

  virtual void callGetTopValue() override {
    //topValue = iter.callGetTopValue();
  }

  virtual std::shared_ptr<cclient::data::Key> getTopKey() override;

  virtual std::shared_ptr<cclient::data::Value> getTopValue() override;


  virtual bool hasTop() override;

  virtual void next() override;

 private:
  IteratorPythonExecutor iter;

};

} /* namespace python */
} /* namespace jni */
} /* namespace cclient */

#endif //_PYTHONITERATOR_
