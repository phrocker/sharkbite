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
#include <mutex>

#include "../Scan.h"
#include "../scanrequest/ScanIdentifier.h"
#include "../scanrequest/ScanRequest.h"
#include "AccumuloServerFacade.h"
#include "data/constructs/security/AuthInfo.h"
#include "data/exceptions/NotServingException.h"
#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/TabletClientService.h"
#include "data/extern/thriftv2/ClientService.h"
#include "data/extern/thriftv2/TabletClientService.h"
#include "data/extern/thriftv2/ThriftV2Wrapper.h"
#include "logging/Logger.h"

namespace interconnect {

class AccumuloServerFacadeV2 : public AccumuloServerFacade {
 private:
  std::shared_ptr<logging::Logger> logger;

  std::map<cclient::data::security::AuthInfo *,
           org::apache::accumulov2::core::securityImpl::thrift::TCredentials>
      convertedMap_V2;

  std::mutex mutex;

  void v2_authenticate(cclient::data::security::AuthInfo *auth);

  Scan *v2_singleScan(
      std::atomic<bool> *isRunning,
      ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                                 std::shared_ptr<cclient::data::Range>>>
          *request);

  Scan *v2_multiScan(
      std::atomic<bool> *isRunning,
      ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                                 std::shared_ptr<cclient::data::Range>>>
          *request);

  Scan *v2_beginScan(
      std::atomic<bool> *isRunning,
      ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                                 std::shared_ptr<cclient::data::Range>>>
          *request);

  void v2_registerService(std::string instance, std::string clusterManagers);

  org::apache::accumulov2::core::securityImpl::thrift::TCredentials
  getOrSetCredentialsV2(cclient::data::security::AuthInfo *convert);

  std::map<std::string, std::string> v2_getNamespaceConfiguration(
      cclient::data::security::AuthInfo *auth,
      const std::string &nameSpaceName);

  Scan *v2_continueMultiScan(Scan *originalScan);

  Scan *v2_continueScan(Scan *originalScan);

  void *v2_write(
      cclient::data::security::AuthInfo *auth,
      std::map<cclient::data::KeyExtent,
               std::vector<std::shared_ptr<cclient::data::Mutation>>> *request);

  bool v2_dropUser(cclient::data::security::AuthInfo *auth, std::string user);

  bool v2_changeUserPassword(cclient::data::security::AuthInfo *auth,
                             std::string user, std::string password);

  bool v2_createUser(cclient::data::security::AuthInfo *auth, std::string user,
                     std::string password);

  std::map<std::string, std::string> v2_getTableConfiguration(
      cclient::data::security::AuthInfo *auth, std::string table);

  cclient::data::security::Authorizations *v2_getUserAuths(
      cclient::data::security::AuthInfo *auth, std::string user);

  void v2_changeUserAuths(cclient::data::security::AuthInfo *auth,
                          std::string user,
                          cclient::data::security::Authorizations *auths);

  void v2_splitTablet(cclient::data::security::AuthInfo *auth,
                      std::shared_ptr<cclient::data::KeyExtent> extent,
                      std::string split);

  void v2_close();

  std::unique_ptr<
      org::apache::accumulov2::core::clientImpl::thrift::ClientServiceClient>
      client_V2;
  std::unique_ptr<org::apache::accumulov2::core::tabletserver::thrift::
                      TabletClientServiceClient>
      tserverClient_V2;

 public:
  AccumuloServerFacadeV2();

  Scan *multiScan(
      std::atomic<bool> *isRunning,
      ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                                 std::shared_ptr<cclient::data::Range>>>
          *request) override;

  Scan *singleScan(
      std::atomic<bool> *isRunning,
      ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                                 std::shared_ptr<cclient::data::Range>>>
          *request) override;

  Scan *beginScan(
      std::atomic<bool> *isRunning,
      ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                                 std::shared_ptr<cclient::data::Range>>>
          *request) override;

  Scan *continueScan(Scan *originalScan) override;

  void *write(cclient::data::security::AuthInfo *auth,
              std::map<cclient::data::KeyExtent,
                       std::vector<std::shared_ptr<cclient::data::Mutation>>>
                  *request) override;

  bool dropUser(cclient::data::security::AuthInfo *auth,
                const std::string &user) override;

  bool changeUserPassword(cclient::data::security::AuthInfo *auth,
                          const std::string &user,
                          const std::string &password) override;

  bool createUser(cclient::data::security::AuthInfo *auth,
                  const std::string &user,
                  const std::string &password) override;

  std::map<std::string, std::string> getTableConfiguration(
      cclient::data::security::AuthInfo *auth,
      const std::string &table) override;

  cclient::data::security::Authorizations *getUserAuths(
      cclient::data::security::AuthInfo *auth,
      const std::string &user) override;

  void changeUserAuths(cclient::data::security::AuthInfo *auth,
                       const std::string &user,
                       cclient::data::security::Authorizations *auths) override;

  void splitTablet(cclient::data::security::AuthInfo *auth,
                   const std::shared_ptr<cclient::data::KeyExtent> extent,
                   std::string split) override;

  void registerService(std::string instance,
                       std::string clusterManagers) override;

  void close() override;

  void initialize(
      std::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr,
      bool callRegistration = true) override;

  std::map<std::string, std::string> getNamespaceConfiguration(
      cclient::data::security::AuthInfo *auth,
      const std::string &nameSpaceName) override;

  void authenticate(cclient::data::security::AuthInfo *auth) override;

  bool hasPermission(cclient::data::security::AuthInfo *auth,
                     const std::string &user,
                     cclient::data::SystemPermissions perm) override;
  bool hasPermission(cclient::data::security::AuthInfo *auth,
                     const std::string &user, const std::string &table,
                     cclient::data::TablePermissions perm) override;
  bool hasPermission(cclient::data::security::AuthInfo *auth,
                     const std::string &user, const std::string &nsp,
                     cclient::data::NamespacePermissions perm) override;
  bool grant(cclient::data::security::AuthInfo *auth, const std::string user,
             cclient::data::SystemPermissions perm) override;
  bool grant(cclient::data::security::AuthInfo *auth, const std::string user,
             const std::string &table,
             cclient::data::TablePermissions perm) override;
  bool grant(cclient::data::security::AuthInfo *auth, const std::string user,
             const std::string &nsp,
             cclient::data::NamespacePermissions perm) override;
  bool revoke(cclient::data::security::AuthInfo *auth, const std::string user,
              cclient::data::SystemPermissions perm) override;
  bool revoke(cclient::data::security::AuthInfo *auth, const std::string user,
              const std::string &table,
              cclient::data::TablePermissions perm) override;
  bool revoke(cclient::data::security::AuthInfo *auth, const std::string user,
              const std::string &nsp,
              cclient::data::NamespacePermissions perm) override;
};

}  // namespace interconnect
