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

#include <utility>

#include "data/constructs/IterInfo.h"
#include "data/constructs/PythonIterInfo.h"

namespace cclient {
namespace data {
namespace python {

/**
 * Constructor
 * @param name iterator name
 * @param cl iterator class name
 * @param pri iterator priority
 **/
PythonIterInfo::PythonIterInfo(const std::string &name, uint32_t pri)
    : cclient::data::IterInfo(name, "", pri, "Python") {
  iterName = name;
  iterClass = "org.poma.accumulo.JythonIterator";
  addOption("DSL_CLASS", name);
  addOption("DSL_NAME", name);
}

PythonIterInfo::PythonIterInfo(const std::string &name, const std::string &idsl,
                               uint32_t pri)
    : cclient::data::IterInfo(name, idsl, pri, "Python"), dsl(idsl) {
  iterName = name;
  iterClass = "org.poma.accumulo.JythonIterator";
  addOption("DSL_CLASS", name);
  addOption("DSL_VALUE", dsl);
  addOption("DSL_NAME", name);
}

/**
 * Destructor
 **/
PythonIterInfo::~PythonIterInfo() {}

PythonIterInfo &PythonIterInfo::onNext(const std::string &expr) {
  if (dsl.empty()) {
    onNextLambda = expr;
    options["DSL_VALUE"] = getDSL();
  } else {
    throw std::runtime_error(
        "Cannot provide -onNext when a python script is provided");
  }
  return *this;
}

/**
 * Returns options
 * @returns options for this iterator
 **/
const std::map<std::string, std::string> PythonIterInfo::getOptions() const {
  auto opts = options;
  opts["DSL_VALUE"] = getDSL();
  return opts;
}

std::string PythonIterInfo::getDSL() const {
  std::string formedDSL = dsl;
  if (formedDSL.empty()) {
    std::stringstream str;
    str << "from org.apache.accumulo.core.data import Range,Value" << std::endl;
    str << "from pysharkbite import Key,KeyValue" << std::endl;

    if (!onNextLambda.empty()) {
      str << "class " << getName() << ":" << std::endl;
      str << "  def seek(self,iterator,range, families, inclusive):"
          << std::endl;
      str << "    iterator.seek(range, families, inclusive) " << std::endl;
      str << "    if (iterator.hasTop()): " << std::endl;
      str << "      key = iterator.getTopKey()" << std::endl;
      str << "      value = iterator.getTopValue()" << std::endl;
      str << "      pkv = KeyValue(key,value)" << std::endl;
      str << "      userfx= " << onNextLambda << std::endl;
      str << "      userrez = userfx(pkv)" << std::endl;
      str << "      if userrez is None:" << std::endl;
      str << "        return None" << std::endl;
      str << "      kv = None" << std::endl;
      str << "      if isinstance(userrez,KeyValue):" << std::endl;
      str << "        kv = KeyValue(userrez.getKey(),userrez.getValue())"
          << std::endl;
      str << "      else:" << std::endl;
      str << "        kv = KeyValue(userrez,pkv.getValue())" << std::endl;
      str << "      iterator.next()" << std::endl;
      str << "      return kv" << std::endl;
      str << "    else:" << std::endl;
      str << "      return None" << std::endl;
      str << "  def onNext(self,iterator):" << std::endl;
      str << "    if (iterator.hasTop()): " << std::endl;
      str << "      key = iterator.getTopKey()" << std::endl;
      str << "      value = iterator.getTopValue()" << std::endl;
      str << "      pkv = KeyValue(key,value)" << std::endl;
      str << "      userfx= " << onNextLambda << std::endl;
      str << "      userrez = userfx(pkv)" << std::endl;
      str << "      if userrez is None:" << std::endl;
      str << "        return None" << std::endl;
      str << "      kv = None" << std::endl;
      str << "      if isinstance(userrez,KeyValue):" << std::endl;
      str << "        kv = KeyValue(userrez.getKey(),userrez.getValue())"
          << std::endl;
      str << "      else:" << std::endl;
      str << "        kv = KeyValue(userrez,pkv.getValue())" << std::endl;
      str << "      iterator.next()" << std::endl;
      str << "      return kv" << std::endl;
      str << "    else:" << std::endl;
      str << "      return None" << std::endl;
    }

    formedDSL = str.str();
  } else {
    std::stringstream str;
    str << "from org.apache.accumulo.core.data import Range,Value" << std::endl;
    str << "from org.poma.accumulo import Key,KeyValue" << std::endl
        << std::endl;
    str << formedDSL;
    formedDSL = str.str();
  }
  return formedDSL;
}
}  // namespace python
}  // namespace data
}  // namespace cclient
