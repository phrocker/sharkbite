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

#ifndef PYTHONITERINFO_H
#define PYTHONITERINFO_H

#include <string>
#include <map>
#include "data/constructs/IterInfo.h"

namespace cclient {
namespace data {
namespace python {

/**
 * Stores iterator information
 **/
class PythonIterInfo : public cclient::data::IterInfo {
 public:
  /**
   * Constructor
   * @param name iterator name
   * @param cl iterator class name
   * @param pri iterator priority
   **/
  explicit PythonIterInfo(const std::string &name, uint32_t pri)
      :
      cclient::data::IterInfo(name, "", pri, "Python") {
  }

  explicit PythonIterInfo(const std::string &name, const std::string &dsl, uint32_t pri)
      :
      cclient::data::IterInfo(name, dsl, pri, "Python"),
      dsl(dsl) {
  }

  /**
   * Destructor
   **/
  virtual ~PythonIterInfo() {

  }

  PythonIterInfo& onNext(const std::string &lambda) {
    if (dsl.empty()){
      onNextLambda = lambda;
      options["DSL_VALUE"] = getDSL();
    }
    else{
      throw std::runtime_error("Cannot provide onNext when a python script is provided");
    }
    return *this;
  }

  /**
   * Returns options
   * @returns options for this iterator
   **/
  const std::map<std::string, std::string> getOptions() const {
    auto opts = options;
    opts["DSL_VALUE"] = getDSL();
    return opts;
  }
 protected:


  std::string getDSL() const {
    std::string formedDSL = dsl;
    if (formedDSL.empty()) {
      std::stringstream str;
      str << "import sharkbite_iterator" << std::endl << std::endl;
      if (!onNextLambda.empty()) {
          str << "def onNext(iterator):" << std::endl;
          str << "  if (iterator.hasTop()): " << std::endl;
          str << "    userfx="<<  onNextLambda << std::endl;
          str << "    key = iterator.getTopKey()" << std::endl;
          str << "    newkey = userfx(key)" << std::endl;
          str << "    kv = sharkbite_iterator.KeyValue()" << std::endl;
          str << "    kv.setKey(newkey,True)" << std::endl;
          str << "    return kv" << std::endl;
          str << "  else:" << std::endl;
          str << "    return None" << std::endl;
      }

      formedDSL = str.str();
    }
    return formedDSL;
  }

  std::string dsl;
  std::string onNextLambda;


};

} /* namespace python */
} /* namespace data */
} /* namespace cclient */

#endif /* PYTHONITERINFO_H */
