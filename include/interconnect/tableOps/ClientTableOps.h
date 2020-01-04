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

#ifndef SRC_INTERCONNECT_TABLEOPS_CLIENTTABLEOPS_H_
#define SRC_INTERCONNECT_TABLEOPS_CLIENTTABLEOPS_H_
#include "TableOperations.h"

#include "../../data/constructs/KeyValue.h"
#include "../../data/constructs/security/AuthInfo.h"
#include "../../data/constructs/client/Instance.h"
#include "../../scanner/Source.h"
#include "../../scanner/constructs/Results.h"
#include "../transport/AccumuloMasterTransporter.h"
#include "../RootInterface.h"
#include "../../writer/Sink.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"

namespace interconnect {

#include <memory>
#include <map>
#include <vector>
#include <string>

/**
 * Accumulo Table Operations;
 */
class AccumuloTableOperations : public interconnect::TableOperations<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> {
 public:
  AccumuloTableOperations(cclient::data::security::AuthInfo *creds, std::shared_ptr<cclient::data::Instance> instance, std::string table,
                          RootInterface<interconnect::AccumuloMasterTransporter, cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *interface,
                          std::shared_ptr<CachedTransport<interconnect::AccumuloMasterTransporter>> tserverConn, TransportPool<interconnect::AccumuloMasterTransporter> *distributedConnector)
      :
      TableOperations<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>(creds, instance, table),
      clientInterface(interface),
      tserverConn(tserverConn),
      distributedConnector(distributedConnector),
      logger(logging::LoggerFactory<AccumuloTableOperations>::getLogger()) {
    loadTableOps();
    getTableId();

  }
  virtual ~AccumuloTableOperations();

  /**
   * Create a table.
   * @param recreate will recreate a table if necessary
   * @return whether or not the table was created.
   **/
  virtual bool create(bool recreate = false) override;

  /**
   * Removes the current table
   * @return whether or not removal was successful.
   **/
  virtual bool remove() override;

  /**
   * Determines if the table exists.
   * @param createIfNot will create the table if true and it does not exist
   * @return returns boolean of whether or not the table exists
   **/
  virtual bool exists(bool createIfNot = false) override;

  /**
   * Imports rfiles in path specified in dir
   * @param dir dir containing rfiles
   * @param fail_path failure path directory
   * @param setTime Accumulo will set the time
   * @return status of create
   **/
  virtual bool import(std::string dir, std::string fail_path, bool setTime = false);

  /**
   * Flushes the current table
   * @param startRow start row on which to begin the flush
   * @param endRow end point for the flush
   * @param wait wait on this operation before returning
   * @return status of flush
   * 0 failure
   * 1 success, flush occurred
   **/
  virtual int8_t flush(std::string startRow, std::string endRow, bool wait);

  /**
   * Compacts the current table
   * @param startRow start row on which to begin the compaction
   * @param endRow end point for the compaction
   * @param wait wait on this operation before returning
   * @return status of compaction
   * 0 failure
   * 1 success, compaction occurred
   **/
  virtual int8_t compact(std::string startRow, std::string endRow, bool wait) override;

  /**
   * Returns the table ID
   * @return table ID
   **/
  std::string getTableId() override;

  /**
   * Sets a table property
   * @param property property names
   * @param value property value
   * -1 property is empty
   * 0 successs
   */
  virtual int8_t setProperty(std::string property, std::string value) override;

  /**
   * Removes a property on this table.
   * @param property property name to remove
   * -1 property is empty
   * success
   */
  virtual int8_t removeProperty(std::string property) override;

  /**
   * Returns all table properties.
   * @return table properties
   */
  virtual std::map<std::string, std::string> getProperties() override;

  /**
   * Returns splits for this table.
   * @return table splits.
   */
  std::vector<std::string> listSplits();

  /**
   * Adds splits for the current table
   * @param partitions table splits
   **/
  void addSplits(std::set<std::string> partitions);

  /**
   * Add a new constraint to the current table.
   * @param constraintClassName full canonical name of the constraint class
   * @return unique number assigned to the constraint
   **/
  int addConstraint(std::string constraintClassName);

  /**
   * Creates a new scanner
   * @param auths authorizations for this scanner
   * @param threads current threads
   * @return new scanner
   **/
  std::unique_ptr<scanners::Source<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>> createScanner(cclient::data::security::Authorizations *auths, uint16_t threads) override;

  /**
   * Creates a writer for the current table
   * @param auths authorizations for this writer
   * @param threads number of threads for writer
   * @return new batch writer
   */
  std::unique_ptr<writer::Sink<cclient::data::KeyValue>> createWriter(cclient::data::security::Authorizations *auths, uint16_t threads) override;

 protected:

  TransportPool<interconnect::AccumuloMasterTransporter> *distributedConnector;

  std::shared_ptr<CachedTransport<interconnect::AccumuloMasterTransporter>> tserverConn;

  RootInterface<interconnect::AccumuloMasterTransporter, cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>> *clientInterface;

  void loadTableOps(bool force = false);

 private:
  std::shared_ptr<logging::Logger> logger;
};

extern std::map<std::string, std::string> nameSpaceIds;

} /* namespace impl */

#endif /* SRC_INTERCONNECT_TABLEOPS_CLIENTTABLEOPS_H_ */
