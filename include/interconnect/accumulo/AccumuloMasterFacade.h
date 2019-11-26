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

#include "AccumuloFacade.h"
#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/TabletClientService.h"
#include <mutex>

#include "data/constructs/InstanceVersion.h"
#include "../scanrequest/ScanRequest.h"
#include "../scanrequest/ScanIdentifier.h"
#include "data/extern/thrift/master_types.h"
#include "data/extern/thrift/MasterClientService.h"
#include "data/extern/thriftv2/MasterClientService.h"
#include "data/extern/thrift/ThriftWrapper.h"
#include "data/extern/thriftv2/ThriftV2Wrapper.h"
#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"
#include "logging/Logger.h"

namespace interconnect {

class AccumuloMasterFacade {

 private:

  std::shared_ptr<logging::Logger> logger;
 protected:
  uint8_t accumuloVersion;
  std::string host;

  std::function<std::shared_ptr<apache::thrift::transport::TTransport>()> createTransport;
  std::function<void()> createMasterTransport;

 public:

  AccumuloMasterFacade(const std::string &host, int version, std::function<void()> fx, std::function<std::shared_ptr<apache::thrift::transport::TTransport>()> tfx)
      : createMasterTransport(fx),
        createTransport(tfx),
        host(host),
        accumuloVersion(version) {
  }

  virtual ~AccumuloMasterFacade() {
  }

  virtual void createMasterClient(std::shared_ptr<apache::thrift::transport::TTransport> underlyingTransport) = 0;

  void recreateMasterTransport() {
    createMasterTransport();
  }

  virtual std::string doFateOperations(cclient::data::security::AuthInfo *auth, AccumuloFateOperation mytype, const std::vector<std::string> &tableArgs,
                                       const std::map<std::string, std::string> &options, bool wait = false) = 0;

  virtual bool createTable(cclient::data::security::AuthInfo *auth, const std::string &table) = 0;

  virtual bool importDirectory(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &dir, std::string failure_dir, bool setTime)= 0;

  virtual bool compactFallBack(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait)= 0;
  virtual bool compact(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) = 0;

  virtual bool flush(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait)= 0;

  virtual bool removeTable(cclient::data::security::AuthInfo *auth, const std::string &table) = 0;

  virtual void removeTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property) = 0;

  virtual void setTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property, const std::string &value) = 0;

  /**namespace operations**/

  virtual bool createNamespace(cclient::data::security::AuthInfo *auth, std::string name) = 0;

  virtual bool deletenamespace(cclient::data::security::AuthInfo *auth, std::string name) = 0;

  virtual bool renamenamespace(cclient::data::security::AuthInfo *auth, std::string oldName, std::string newName) = 0;

  virtual void removeNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property)= 0;

  virtual void setNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property, const std::string &value)= 0;

};

}
