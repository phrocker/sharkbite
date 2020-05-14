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

#ifndef SCANNER_H_
#define SCANNER_H_

#include <algorithm>
#include <map>
#include <memory>
#include <string>
#include <set>
#include <mutex>

#include "../Source.h"
#include "../../data/constructs/Key.h"
#include "../../data/constructs/security/AuthInfo.h"
#include "../../data/constructs/security/Authorizations.h"
#include "../../data/constructs/value.h"
#include "../constructs/Results.h"
#include "../../data/constructs/inputvalidation.h"
#include "../../data/client/ExtentLocator.h"
#include "../../data/exceptions/ClientException.h"
#include "../../data/constructs/client/zookeeperinstance.h"
#include "../../data/client/LocatorCache.h"
#include "../constructs/ServerHeuristic.h"
#include "../../interconnect/ClientInterface.h"
#include "../../interconnect/tableOps/TableOperations.h"
#include "logging/LoggerConfiguration.h"
#include "logging/Logger.h"
#include "scanner/ScannerOptions.h"
#include "data/constructs/IterInfo.h"
#include "scanner/constructs/HedgedHeuristic.h"

namespace scanners {

/**
 Scanner scans uses the interconnect to retrieve Keys and Values from the data source.
 **/
class Scanner : public scanners::Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> {
 public:
  /**
   * Scanner constructor
   * @param instance connector instance
   * @param top table operations refereuce
   * @param auths authorizations
   * @param threads threads for the scanner
   **/
  explicit Scanner(std::shared_ptr<cclient::data::Instance> instance, interconnect::TableOperations<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *tops,
                   cclient::data::security::Authorizations *auths, uint16_t threads);

  virtual void addRange(const cclient::data::Range &range) override;
  /**
   * Adds a range to the scanner
   * @param range
   **/
  virtual void addRange(std::unique_ptr<cclient::data::Range> range) override;

  /**
   * Returns a result set.
   * @return results iterator.
   **/
  Results<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>>* getResultSet() override;

  void addResults(Results<std::shared_ptr<cclient::data::KeyValue>, ResultBlock<std::shared_ptr<cclient::data::KeyValue>>> *results) {

  }

  virtual ~Scanner() {
    if (!IsEmpty(resultSet)) {
      delete resultSet;
    }
  }

  std::shared_ptr<cclient::data::Instance> getInstance() override {
    return connectorInstance;
  }

  void locateFailedTablet(std::vector<std::shared_ptr<cclient::data::Range>> ranges, std::vector<std::shared_ptr<cclient::data::tserver::RangeDefinition>> *locatedTablets) override;

  void close() override {
    scannerHeuristic->close();
  }

  virtual void setOption(ScannerOptions opt) override;

  virtual void removeOption(ScannerOptions opt) override ;

 protected:


  /**
   * Flushes the scanner
   * @param override ensures that flushes occur despite not meeting requirements
   **/
  void flush(bool override = false);

  /**
   * Adds a connecting server definition
   * @param ifc server definition
   **/
  void addServerDefinition(interconnect::ClientInterface<interconnect::ThriftTransporter> *ifc) {
    servers.push_back(ifc);
  }

  /**
   * Adds a vector of connecting server definition
   * @param ifc server definition
   **/
  void addServerDefinition(std::vector<interconnect::ClientInterface<interconnect::ThriftTransporter>*> ifc) {
    servers.insert(servers.end(), ifc.begin(), ifc.end());
  }

  std::vector<cclient::data::IterInfo> getTableIterators(std::string iterName="");

  uint16_t numThreads;
  // scanner
  std::mutex scannerLock;
  // vector of ranges to interrogate.
  std::vector<std::shared_ptr<cclient::data::Range>> ranges;
  // result set iterator
  Results<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *resultSet;
  // credentials
  cclient::data::security::AuthInfo *credentials;
  // scanner authorizations
  cclient::data::security::Authorizations *scannerAuths;
  // servers to access
  std::vector<interconnect::ClientInterface<interconnect::ThriftTransporter>*> servers;
  // zookeeper instance
  std::shared_ptr<cclient::data::Instance> connectorInstance;
  // scanner heuristic to control server access
  std::unique_ptr<ScannerHeuristic> scannerHeuristic;
  // tablet locator
  cclient::impl::TabletLocator *tableLocator;

 private:
  std::shared_ptr<logging::Logger> logger;
};

template<class T>
using Iterator = Results<T, ResultBlock<T>>;

using BatchScanner =scanners::Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>>;
}
#endif /* SCANNER_H_ */

