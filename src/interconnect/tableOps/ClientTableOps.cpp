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

#include <string>

#include "../../../include/interconnect/tableOps/ClientTableOps.h"
#include "../../../include/interconnect/tableOps/TableOperations.h"
#include "../../../include/interconnect/tableOps/../accumulo/AccumuloStreams.h"
#include "../../../include/interconnect/tableOps/../../scanner/impl/Scanner.h"
#include "../../../include/interconnect/tableOps/../../writer/impl/SinkImpl.h"
#include "../../../include/interconnect/tableOps/../../data/constructs/ConfigurationOptions.h"
#include "../../../include/interconnect/tableOps/../transport/AccumuloMasterTransporter.h"
#include "../../../include/data/client/LocatorCache.h"
#include "../../../include/data/client/ExtentLocator.h"

namespace interconnect {

AccumuloTableOperations::~AccumuloTableOperations() {
  // we don't own client interface.
//	delete clientInterface;
  distributedConnector->freeTransport(tserverConn);
}

std::unique_ptr<
    scanners::Source<cclient::data::KeyValue,
        scanners::ResultBlock<cclient::data::KeyValue>>> AccumuloTableOperations::createScanner(
    cclient::data::security::Authorizations *auths, uint16_t threads) {
  if (IsEmpty(auths))
    throw cclient::exceptions::ClientException(ARGUMENT_CANNOT_BE_NULL);
  if (!exists())
    throw cclient::exceptions::ClientException(TABLE_NOT_FOUND);
  return std::unique_ptr<
      scanners::Source<cclient::data::KeyValue,
          scanners::ResultBlock<cclient::data::KeyValue>>>(
      new AccumuloStreams(myInstance, this, auths, threads));
}

std::unique_ptr<writer::Sink<cclient::data::KeyValue>> AccumuloTableOperations::createWriter(
    cclient::data::security::Authorizations *auths, uint16_t threads) {
  if (!exists())
    throw cclient::exceptions::ClientException(TABLE_NOT_FOUND);
  return std::unique_ptr<writer::Sink<cclient::data::KeyValue>>(
      new AccumuloStreams(myInstance, this, auths, threads));

}

void AccumuloTableOperations::loadTableOps(bool force) {
  std::lock_guard<std::recursive_mutex> lock(tableOpMutex);
  const cclient::impl::Configuration *conf = myInstance->getConfiguration();

  std::string fsRoot = conf->get(FILE_SYSTEM_ROOT_CFG);

  fsRoot.append("/");
  fsRoot.append(myInstance->getInstanceId());
  fsRoot.append( TABLE_SUFFIX);

  cclient::data::InstanceCache *cache = myInstance->getInstanceCache();
  std::vector<std::string> tableIds = cache->getChildren(fsRoot, force);

  cachedTableIds.clear();
  tableNames.clear();
  for (std::string retrievedId : tableIds) {
    std::string tablePath = fsRoot;
    tablePath.append("/");
    tablePath.append(retrievedId);

    std::string namePath = tablePath;
    namePath.append(TABLE_GET_NAME);
    char *path = (char*) cache->getData(namePath);
    if (IsEmpty(path)) {

      loadTableOps();

      return;

    }
    std::string tableName = std::string(path);

    std::string nameSpacePath = tablePath;
    nameSpacePath.append(TABLE_GET_NAMESPACE);
    char *nmpath = (char*) cache->getData(nameSpacePath);

    std::string namespaceId = "";

    if (!IsEmpty(nmpath))
      namespaceId = nmpath;

    std::string namespaceName = "";
    if (!IsEmpty(&namespaceId)) {
      if (namespaceId != DEFAULT_NAMESPACE_ID) {
      }
    }

    if (!IsEmpty(&tableName)) {
      // insert both representations
      cachedTableIds.insert(std::make_pair(retrievedId, tableName));
      cachedTableIds.insert(std::make_pair(tableName, retrievedId));
      tableNames.insert(tableName);
    }

  }
}

std::string AccumuloTableOperations::getTableId() {
  std::string retTable = "";
  std::lock_guard<std::recursive_mutex> lock(tableOpMutex);
  if (IsEmpty(&tableId)) {

    std::string ephemeralTableId = "";

    try {
      cachedTableIds.at(myTable);
    } catch (const std::out_of_range& e) {
      // do nothing since it isn't cached
    }
    // it's not cached!!!
    if (IsEmpty(&ephemeralTableId)) {
      loadTableOps();
      try {
        ephemeralTableId = cachedTableIds.at(myTable);
      } catch (std::out_of_range o) {
        // table does not exist
        // do nothing
      }

    }
    tableId = ephemeralTableId;
  }
  retTable = tableId;
  return retTable;
}

bool AccumuloTableOperations::import(std::string dir, std::string fail_path,
                                     bool setTime) {
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface
      ->getTransport().get();

  if (!baseTransport->importDirectory(credentials, myTable, dir, fail_path,
                                      setTime)) {
    return false;
  }

  return true;
}

int8_t AccumuloTableOperations::flush(std::string startRow, std::string endRow,
                                      bool wait) {
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface
      ->getTransport().get();

  if (!baseTransport->flush(credentials, tableId, startRow, endRow, wait)) {
    return 0;
  }

  return 1;
}

int8_t AccumuloTableOperations::compact(std::string startRow,
                                        std::string endRow, bool wait) {
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface
      ->getTransport().get();

  if (!baseTransport->compact(credentials, tableId, startRow, endRow, wait)) {
    return 0;
  }

  return 1;
}

bool AccumuloTableOperations::create(bool recreate) {
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface
      ->getTransport().get();

  std::lock_guard<std::recursive_mutex> lock(tableOpMutex);
  if (recreate) {
    remove();
  }
  if (!baseTransport->createTable(credentials, myTable)) {
    return false;
  }
  tableId = "";
  loadTableOps();

  return true;
}

std::vector<std::string> AccumuloTableOperations::listSplits() {
  if (IsEmpty(&tableId)) {
    loadTableOps();
  }

  cclient::data::zookeeper::ZookeeperInstance *connectorInstance =
      dynamic_cast<cclient::data::zookeeper::ZookeeperInstance*>(myInstance);

  cclient::impl::TabletLocator *tabletLocator = cclient::impl::cachedLocators
      .getLocator(cclient::impl::LocatorKey(connectorInstance, tableId));

  std::vector<cclient::data::TabletLocation> locations = tabletLocator
      ->locations(credentials);

  std::vector<std::string> tablets;
  for (auto location : locations) {
    std::shared_ptr<cclient::data::KeyExtent> extent = location.getExtent();
    tablets.push_back(extent->getEndRow());

  }
  return tablets;
}

void AccumuloTableOperations::addSplits(std::set<std::string> partitions) {
  cclient::data::zookeeper::ZookeeperInstance *connectorInstance =
      dynamic_cast<cclient::data::zookeeper::ZookeeperInstance*>(myInstance);

  // need better error handling here
  for (std::string partition : partitions) {
    bool success = false;
    while (!success) {
      cclient::impl::TabletLocator *tabletLocator =
          cclient::impl::cachedLocators.getLocator(
              cclient::impl::LocatorKey(connectorInstance, tableId));
      cclient::data::TabletLocation location = tabletLocator->locateTablet(
          credentials, partition, false, false);

      std::shared_ptr<ServerConnection> connection = std::make_shared<
          ServerConnection>(location.getServer(), location.getPort(), -1);

      auto cachedTransport =
          distributedConnector->getTransporter(connection);

      try {

        cachedTransport->getTransport()->splitTablet(credentials,
                                                     location.getExtent(),
                                                     partition);
        success = true;
      } catch (apache::thrift::protocol::TProtocolException tpe) {
        cachedTransport->sawError(true);
        distributedConnector->freeTransport(cachedTransport);
        success = false;
      } catch (apache::thrift::transport::TTransportException tpe) {
        cachedTransport->sawError(true);
        distributedConnector->freeTransport(cachedTransport);
        success = false;
      }

      distributedConnector->freeTransport(cachedTransport);

    }
  }
}

int8_t AccumuloTableOperations::removeProperty(std::string property) {

  if (IsEmpty(&property))
    return -1;
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface
      ->getTransport().get();
  baseTransport->removeTableProperty(credentials, myTable, property);

  return 0;

}

int8_t AccumuloTableOperations::setProperty(std::string property,
                                            std::string value) {
  if (IsEmpty(&property))
    return -1;
  if (IsEmpty(&value))
    return removeProperty(property);
  interconnect::AccumuloMasterTransporter *baseTransport = clientInterface
      ->getTransport().get();
  baseTransport->setTableProperty(credentials, myTable, property, value);
  return 0;
}

std::map<std::string, std::string> AccumuloTableOperations::getProperties() {
  auto baseTransport = tserverConn->getTransport();

  return baseTransport->getTableConfiguration(credentials, myTable);
}

bool AccumuloTableOperations::remove() {
  auto baseTransport = clientInterface
      ->getTransport();

  std::lock_guard<std::recursive_mutex> lock(tableOpMutex);
  if (!baseTransport->removeTable(credentials, myTable)) {
    return false;
  }
  tableId = "";
  loadTableOps(true);

  return true;
}

bool AccumuloTableOperations::exists(bool createIfNot) {
  loadTableOps(true);
  bool contains = cachedTableIds.find(myTable) != std::end(cachedTableIds);

  if (!contains && createIfNot) {
    if (create())
      return true;
  }
  return contains;
}

int AccumuloTableOperations::addConstraint(std::string constraintClassName) {
  // set the table prop for the constraint
  return 0;
}

std::map<std::string, std::string> nameSpaceIds;
} /* namespace impl */
