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

#ifndef ITERINFO_H_
#define ITERINFO_H_

#include <string>
#include <map>
#include "inputvalidation.h"

namespace cclient {
namespace data {

enum class ITERATOR_TYPES {
  majc = 1,
  minc = 2,
  scan = 4

};

typedef Iterator<ITERATOR_TYPES, ITERATOR_TYPES::majc, ITERATOR_TYPES::scan> TypeIterator;

inline ITERATOR_TYPES operator|(ITERATOR_TYPES a, ITERATOR_TYPES b) {
  return static_cast<ITERATOR_TYPES>(static_cast<int>(a) | static_cast<int>(b));
}

inline std::ostream& operator <<(std::ostream &out, const ITERATOR_TYPES &obj) {
  switch (obj) {
    case ITERATOR_TYPES::majc:
      out << "majc";
      break;
    case ITERATOR_TYPES::minc:
      out << "minc";
      break;
    case ITERATOR_TYPES::scan:
      out << "majc";
      break;

  }
  ;
  return out;
}

/**
 * Stores iterator information
 **/
class IterInfo {
 public:
  IterInfo()
      :
      priority(0) {

  }
  /**
   * Constructor
   * @param name iterator name
   * @param cl iterator class name
   * @param pri iterator priority
   **/
  explicit IterInfo(const std::string &name, const std::string &cl, uint32_t pri)
      :
      iterName(name),
      iterClass(cl),
      priority(pri) {

  }

  /**
   * Constructor
   * @param name iterator name
   * @param cl iterator class name
   * @param pri iterator priority
   **/
  explicit IterInfo(const std::string &name, const std::string &cl, uint32_t pri, std::map<std::string, std::string> &opts)
      :
      IterInfo(name, cl, pri) {
    options = opts;
  }

  explicit IterInfo(const std::string &name, const std::string &dsl, uint32_t pri, const std::string &type)
      :
      iterName("NativeDSLIterator"),
      iterClass("org.poma.accumulo.NativeDSLIterator"),
      priority(pri) {
    addOption("DSL_TYPE", type);
    addOption("DSL_VALUE", dsl);
    addOption("DSL_NAME", name);
  }
  /**
   * Destructor
   **/
  virtual ~IterInfo() {

  }

  /**
   * Returns the priority
   * @returns priority of this iterator
   **/
  uint32_t getPriority() const {
    return priority;
  }

  /**
   * Returns true if the iterator info object is empty
   */
  bool empty() const {
    return iterName.empty() && iterClass.empty();
  }

  /**
   * Returns the iterator name
   * @returns name of this iterator
   **/
  std::string getName() const {
    return iterName;
  }

  /**
   * Returns the class
   * @returns classname of this iterator
   **/
  std::string getClass() const {
    return iterClass;
  }

  /**
   * Adds an option to the iterator map
   * @param optionName name of the option
   * @param option value
   * @returns result
   **/
  virtual bool addOption(std::string optionName, std::string optionValue) {
    if (optionName.empty()) {
      return false;
    }
    options[optionName] = optionValue;
    return true;
  }

  /**
   * Returns options
   * @returns options for this iterator
   **/
  std::string getOption(const std::string &value, const std::string &def = "") const {
    auto fnd = options.find(value);
    if (fnd != std::end(options)) {
      return fnd->second;
    } else {
      return def;
    }
  }

  /**
   * Returns options
   * @returns options for this iterator
   **/
  virtual const std::map<std::string, std::string> getOptions() const {
    return options;
  }
 protected:

  // options map
  std::map<std::string, std::string> options;
  // priority for this iterator
  uint32_t priority;
  // name for this iterator
  std::string iterName;
  // canonical class name
  std::string iterClass;
};

} /* namespace data */
} /* namespace cclient */
#endif /* ITERINFO_H_ */
