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
  explicit PythonIterInfo(const std::string &name, uint32_t pri);

  explicit PythonIterInfo(const std::string &name, const std::string &idsl, uint32_t pri);

  /**
   * Destructor
   **/
  virtual ~PythonIterInfo();

  PythonIterInfo& onNext(const std::string &lambda);

  /**
   * Returns options
   * @returns options for this iterator
   **/
  const std::map<std::string, std::string> getOptions() const override;
 protected:

  std::string getDSL() const;

  std::string dsl;
  std::string onNextLambda;

};

} /* namespace python */
} /* namespace data */
} /* namespace cclient */

#endif /* PYTHONITERINFO_H */
