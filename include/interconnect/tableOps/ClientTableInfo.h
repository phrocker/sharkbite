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

#pragma once

#include "../../data/constructs/KeyValue.h"
#include "../../data/constructs/client/Instance.h"
#include "../../data/constructs/security/AuthInfo.h"
#include "../../scanner/Source.h"
#include "../../scanner/constructs/Results.h"
#include "../../writer/Sink.h"
#include "../RootInterface.h"
#include "../transport/AccumuloCoordinatorTransporter.h"
#include "ClientTableOps.h"
#include "TableOperations.h"
#include "data/constructs/ConfigurationOptions.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"

namespace interconnect {

#include <map>
#include <memory>
#include <string>
#include <vector>

/**
 * Accumulo Table Operations;
 */
class AccumuloTableInfo {
 public:
  explicit AccumuloTableInfo(cclient::data::security::AuthInfo *creds,
                             std::shared_ptr<cclient::data::Instance> instance)
      : credentials(creds),
        myInstance(instance),
        logger(logging::LoggerFactory<AccumuloTableInfo>::getLogger()) {
    loadTableOps();
  }
  virtual ~AccumuloTableInfo() {}

  /**
   * Determines if the table exists.
   * @return returns boolean of whether or not the table exists
   **/
  bool exists(const std::string table) {
    return Tables::getInstance().exists(table);
  }

  /**
   * Returns the table ID for the given table name
   * @return table ID
   **/
  std::string getTableId(const std::string &table) {
    return Tables::getInstance().getTableId(table);
  }

  /**
   * Returns the table name for the table id
   * @param tableid
   * @return table name
   **/
  std::string getTableName(const std::string &tableid) {
    return Tables::getInstance().getTableName(tableid);
  }

 private:
  void loadTableOps() {
    const cclient::impl::Configuration *conf = myInstance->getConfiguration();

    bool force = true;

    std::string fsRoot = conf->get(FILE_SYSTEM_ROOT_CFG, "/accumulo");

    fsRoot.append("/");
    fsRoot.append(myInstance->getInstanceId());
    fsRoot.append(TABLE_SUFFIX);

    cclient::data::InstanceCache *cache = myInstance->getInstanceCache();
    std::vector<std::string> tableIds = cache->getChildren(fsRoot, force);

    std::map<std::string, std::string> cachedTableIds;
    std::set<std::string> tableNames;
    for (std::string retrievedId : tableIds) {
      std::string tablePath = fsRoot;
      tablePath.append("/");
      tablePath.append(retrievedId);

      std::string namePath = tablePath;
      namePath.append(TABLE_GET_NAME);
      char *path = (char *)cache->getData(namePath);
      if (IsEmpty(path)) {
        loadTableOps();

        return;
      }
      std::string tableName = std::string(path);

      std::string nameSpacePath = tablePath;
      nameSpacePath.append(TABLE_GET_NAMESPACE);
      char *nmpath = (char *)cache->getData(nameSpacePath);

      std::string namespaceId = "";

      if (!IsEmpty(nmpath)) namespaceId = nmpath;

      std::string namespaceName = "";
      if (!IsEmpty(&namespaceId)) {
        if (namespaceId != DEFAULT_NAMESPACE_ID) {
          auto nm = namespaces.find(namespaceId);
          if (nm != std::end(namespaces)) {
            namespaceName = nm->second;
          }
          // get the namespace name
        }
      }

      if (!IsEmpty(&tableName)) {
        if (tableName.find(".") == std::string::npos &&
            !namespaceName.empty()) {
          tableName = namespaceName + "." + tableName;
        }
        // insert both representations
        cachedTableIds.insert(std::make_pair(retrievedId, tableName));
        cachedTableIds.insert(std::make_pair(tableName, retrievedId));
        tableNames.insert(tableName);
      }
    }

    cachedTableIds.insert(std::make_pair("!0", "accumulo.metadata"));
    cachedTableIds.insert(std::make_pair("accumulo.metadata", "!0"));
    tableNames.insert("accumulo.metadata");

    Tables::getInstance().set(cachedTableIds, tableNames);
  }

  void loadNamespaces() {
    bool force = true;
    const cclient::impl::Configuration *conf = myInstance->getConfiguration();

    std::string fsRoot = conf->get(FILE_SYSTEM_ROOT_CFG, "/accumulo");

    fsRoot.append("/");
    fsRoot.append(myInstance->getInstanceId());
    fsRoot.append(TABLE_GET_NAMESPACES);

    cclient::data::InstanceCache *cache = myInstance->getInstanceCache();
    std::vector<std::string> namespaceIds = cache->getChildren(fsRoot, force);

    namespaces.clear();
    namespaceNames.clear();
    for (std::string retrievedId : namespaceIds) {
      std::string tablePath = fsRoot;
      tablePath.append("/");
      tablePath.append(retrievedId);

      std::string namePath = tablePath;
      namePath.append(TABLE_GET_NAME);
      char *path = (char *)cache->getData(namePath);
      if (IsEmpty(path)) {
        continue;
      }
      std::string namespaceName = std::string(path);

      if (!IsEmpty(&namespaceName)) {
        // insert both representations
        namespaces.insert(std::make_pair(retrievedId, namespaceName));
        namespaces.insert(std::make_pair(namespaceName, retrievedId));
        namespaceNames.push_back(namespaceName);
      }
    }
  }

  std::map<std::string, std::string> namespaces;
  std::vector<std::string> namespaceNames;

  std::shared_ptr<cclient::data::Instance> myInstance;
  // credentials of the calling user
  cclient::data::security::AuthInfo *credentials;
  std::shared_ptr<logging::Logger> logger;
};

}  // namespace interconnect
