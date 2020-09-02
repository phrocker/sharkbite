/*
 * Licensed under the Apache License, Version 2.0 (the "License") = 0;
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
#include <mutex>

#include "../Scan.h"
#include "../scanrequest/ScanIdentifier.h"
#include "../scanrequest/ScanRequest.h"
#include "AccumuloFacade.h"
#include "data/constructs/security/AuthInfo.h"
#include "data/constructs/security/Permissions.h"
#include "data/extern/thrift/ThriftWrapper.h"
#include "data/extern/thriftv2/ThriftV2Wrapper.h"
#include "logging/Logger.h"

namespace interconnect {

class AccumuloServerFacade {
 private:
  std::shared_ptr<logging::Logger> logger;

 protected:
  uint8_t accumuloVersion;

 public:
  explicit AccumuloServerFacade(int version);
  virtual ~AccumuloServerFacade() {}

  virtual Scan *multiScan(
      std::atomic<bool> *isRunning,
      ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                                 std::shared_ptr<cclient::data::Range>>>
          *request) = 0;

  virtual Scan *singleScan(
      std::atomic<bool> *isRunning,
      ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                                 std::shared_ptr<cclient::data::Range>>>
          *request) = 0;

  virtual Scan *beginScan(
      std::atomic<bool> *isRunning,
      ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                                 std::shared_ptr<cclient::data::Range>>>
          *request) = 0;

  virtual Scan *continueScan(Scan *originalScan) = 0;

  virtual void *write(
      cclient::data::security::AuthInfo *auth,
      std::map<cclient::data::KeyExtent,
               std::vector<std::shared_ptr<cclient::data::Mutation>>>
          *request) = 0;

  virtual bool dropUser(cclient::data::security::AuthInfo *auth,
                        const std::string &user) = 0;

  virtual bool changeUserPassword(cclient::data::security::AuthInfo *auth,
                                  const std::string &user,
                                  const std::string &password) = 0;

  virtual bool createUser(cclient::data::security::AuthInfo *auth,
                          const std::string &user,
                          const std::string &password) = 0;

  virtual std::map<std::string, std::string> getTableConfiguration(
      cclient::data::security::AuthInfo *auth, const std::string &table) = 0;

  virtual cclient::data::security::Authorizations *getUserAuths(
      cclient::data::security::AuthInfo *auth, const std::string &user) = 0;

  virtual void changeUserAuths(
      cclient::data::security::AuthInfo *auth, const std::string &user,
      cclient::data::security::Authorizations *auths) = 0;

  virtual void splitTablet(
      cclient::data::security::AuthInfo *auth,
      const std::shared_ptr<cclient::data::KeyExtent> extent,
      std::string split) = 0;

  virtual void registerService(std::string instance,
                               std::string clusterManagers) = 0;

  virtual void close() = 0;

  virtual void initialize(
      std::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr,
      bool callRegistration = true) = 0;

  virtual std::map<std::string, std::string> getNamespaceConfiguration(
      cclient::data::security::AuthInfo *auth,
      const std::string &nameSpaceName) = 0;

  virtual void authenticate(cclient::data::security::AuthInfo *auth) = 0;

  /**
   * Permissions
   */

  virtual bool hasPermission(cclient::data::security::AuthInfo *auth,
                             const std::string &user,
                             cclient::data::SystemPermissions perm) = 0;
  virtual bool hasPermission(cclient::data::security::AuthInfo *auth,
                             const std::string &user, const std::string &table,
                             cclient::data::TablePermissions perm) = 0;
  virtual bool hasPermission(cclient::data::security::AuthInfo *auth,
                             const std::string &user, const std::string &nsp,
                             cclient::data::NamespacePermissions perm) = 0;
  virtual bool grant(cclient::data::security::AuthInfo *auth,
                     const std::string user,
                     cclient::data::SystemPermissions perm) = 0;
  virtual bool grant(cclient::data::security::AuthInfo *auth,
                     const std::string user, const std::string &table,
                     cclient::data::TablePermissions perm) = 0;
  virtual bool grant(cclient::data::security::AuthInfo *auth,
                     const std::string user, const std::string &nsp,
                     cclient::data::NamespacePermissions perm) = 0;
  virtual bool revoke(cclient::data::security::AuthInfo *auth,
                      const std::string user,
                      cclient::data::SystemPermissions perm) = 0;
  virtual bool revoke(cclient::data::security::AuthInfo *auth,
                      const std::string user, const std::string &table,
                      cclient::data::TablePermissions perm) = 0;
  virtual bool revoke(cclient::data::security::AuthInfo *auth,
                      const std::string user, const std::string &nsp,
                      cclient::data::NamespacePermissions perm) = 0;
};

}  // namespace interconnect
