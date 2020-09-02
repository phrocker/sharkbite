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

#ifndef SRC_INTERCONNECT_TABLEOPS_TABLEOPERATIONS_H_
#define SRC_INTERCONNECT_TABLEOPS_TABLEOPERATIONS_H_

#include "../../data/constructs/client/Instance.h"
#include "../../data/constructs/inputvalidation.h"
#include "../../scanner/Source.h"
#include "../../writer/Sink.h"

namespace interconnect {

#include <map>
#include <memory>
#include <mutex>
#include <string>

extern std::set<std::string> tableNames;

template <typename K, typename V>
class TableOperations {
 public:
  /**
   * Table Operations constructur
   * @param creds credentials
   * @param instance instance to use for connection
   * @param table table name
   **/
  TableOperations(cclient::data::security::AuthInfo *creds,
                  std::shared_ptr<cclient::data::Instance> instance,
                  std::string table);

  /**
   * Create a table.
   * @param recreate will recreate a table if necessary
   * @return whether or not the table was created.
   **/
  void createTable(std::string table);

  /**
   * Creates a new scanner
   * @param auths authorizations for this scanner
   * @param threads current threads
   * @return new scanner
   **/
  virtual std::unique_ptr<scanners::Source<K, V>> createScanner(
      cclient::data::security::Authorizations *auths, uint16_t threads) = 0;

  /**
   * Creates a writer for the current table
   * @param auths authorizations for this writer
   * @param threads number of threads for writer
   * @return new batch writer
   */
  virtual std::unique_ptr<writer::Sink<K>> createWriter(
      cclient::data::security::Authorizations *auths, uint16_t threads) = 0;

  /**
   * Creates a new scanner
   * @param auths authorizations for this scanner
   * @param threads current threads
   * @return new scanner
   **/
  virtual std::shared_ptr<scanners::Source<K, V>> createSharedScanner(
      cclient::data::security::Authorizations *auths, uint16_t threads) = 0;

  /**
   * Creates a writer for the current table
   * @param auths authorizations for this writer
   * @param threads number of threads for writer
   * @return new batch writer
   */
  virtual std::shared_ptr<writer::Sink<K>> createSharedWriter(
      cclient::data::security::Authorizations *auths, uint16_t threads) = 0;

  virtual ~TableOperations();

  /**
   * Returns the table ID
   * @return table ID
   **/
  virtual std::string getTableId() = 0;

  /**
   * Create a table.
   * @param recreate will recreate a table if necessary
   * @return whether or not the table was created.
   **/
  virtual bool create(bool recreate = false) = 0;

  /**
   * Removes the current table
   * @return whether or not removal was successful.
   **/
  virtual bool remove() = 0;

  /**
   * Determines if the table exists.
   * @param createIfNot will create the table if true and it does not exist
   * @return returns boolean of whether or not the table exists
   **/
  virtual bool exists(bool createIfNot = false) = 0;

  /**
   * Sets a table property
   * @param property property names
   * @param value property value
   */
  virtual int8_t setProperty(std::string property, std::string value) = 0;

  /**
   * Removes a property on this table.
   * @param property property name to remove
   */
  virtual int8_t removeProperty(std::string property) = 0;

  /**
   * Compacts the current table
   * @param startRow start row on which to begin the compaction
   * @param endRow end point for the compaction
   * @param wait wait on this operation before returning
   * @return status of compaction
   **/
  virtual int8_t compact(std::string startRow, std::string endRow,
                         bool wait) = 0;

  /**
   * Returns all table properties.
   * @return table properties
   */
  virtual std::map<std::string, std::string> getProperties() = 0;

  /**
   * Lists tables
   * @return tables
   **/
  virtual std::set<std::string> listTables() { return tableNames; }

  /**
   * Gets user credentials
   * @return credentials
   **/
  cclient::data::security::AuthInfo *getCredentials() { return credentials; }

 protected:
  // table mutex
  std::recursive_mutex tableOpMutex;
  // instance pointer
  std::shared_ptr<cclient::data::Instance> myInstance;
  // credentials of the calling user
  cclient::data::security::AuthInfo *credentials;
  // current table id
  std::string tableId;
  // current table name
  std::string myTable;
};

extern std::map<std::string, std::string> cachedTableIds;

/**
 * Create a table.
 * @param recreate will recreate a table if necessary
 * @return whether or not the table was created.
 **/
template <typename K, typename V>
void TableOperations<K, V>::createTable(std::string table) {}
/**
 * Table Operations constructur
 * @param creds credentials
 * @param instance instance to use for connection
 * @param table table name
 **/
template <typename K, typename V>
TableOperations<K, V>::TableOperations(
    cclient::data::security::AuthInfo *creds,
    std::shared_ptr<cclient::data::Instance> instance, std::string table)
    : myInstance(instance), myTable(table), credentials(creds) {}

template <typename K, typename V>
TableOperations<K, V>::~TableOperations() {}
}  // namespace interconnect
#endif /* SRC_INTERCONNECT_TABLEOPS_TABLEOPERATIONS_H_ */
