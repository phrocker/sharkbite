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
#ifndef SOURCE_H_
#define SOURCE_H_

#include <string>
#include <cstdint>
#include <vector>

#include "./constructs/Results.h"
#include "../data/constructs/client/Instance.h"
#include "../data/constructs/server/ServerDefinition.h"
#include "../data/constructs/server/RangeDefinition.h"
#include "../data/constructs/IterInfo.h"
#include "data/constructs/PythonIterInfo.h"
#include "../data/constructs/column.h"

namespace scanners {

template<typename T, class BlockType>
class Source {
 public:
  Source() {
  }

  virtual void addRange(std::unique_ptr<cclient::data::Range> range) = 0;

  virtual void addRange(const cclient::data::Range &range) = 0;

  /**
   * Add the list of user supplied Iterators;
   */
  void addIterators(const std::vector<cclient::data::IterInfo> &iterV) {
    iters.insert(iters.end(), iterV.begin(), iterV.end());
  }

  void addIterator(const cclient::data::IterInfo &iterV) {
    iters.emplace_back(iterV);
  }

  void addPythonIterator(const cclient::data::python::PythonIterInfo &iterV) {
    iters.emplace_back(iterV);
  }


  virtual void locateFailedTablet(std::vector<std::shared_ptr<cclient::data::Range>> ranges, std::vector<std::shared_ptr<cclient::data::tserver::RangeDefinition>> *locatedTablets) = 0;

  virtual Results<T, BlockType>* getResultSet() = 0;

  //virtual void addResults(Results<T, BlockType> *results) = 0;

  virtual ~Source() {
  }

  virtual void close() {
  }

  std::vector<cclient::data::Column> getColumns() const {
    return columns;
  }

  std::vector<cclient::data::IterInfo> getIters() const {
    return iters;
  }

  virtual cclient::data::Instance* getInstance() = 0;

  void fetchColumn(std::string col, std::string colqual = "") {
    if (!IsEmpty(&colqual)) {
      columns.emplace_back(cclient::data::Column(col, colqual));
    } else
      columns.emplace_back(cclient::data::Column(col));
  }
 protected:
  std::vector<cclient::data::Column> columns;
  std::vector<cclient::data::IterInfo> iters;
};
}
#endif /* SCANNER_H_ */
