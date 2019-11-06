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

#include <boost/concept_check.hpp>
#include <boost/shared_ptr.hpp>
#include "data/extern/boost/SharedPointer.h"
#include "data/extern/thrift/ClientService.h"
#include "data/extern/thriftv2/ClientService.h"
#include "data/exceptions/NotServingException.h"
#include "data/extern/thrift/TabletClientService.h"
#include "data/extern/thriftv2/TabletClientService.h"
#include "../scanrequest/ScanRequest.h"
#include "../scanrequest/ScanIdentifier.h"
#include "data/extern/thrift/ThriftWrapper.h"
#include "data/extern/thriftv2/ThriftV2Wrapper.h"
#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"

namespace interconnect {

class AccumuloServerFacade {

 private:

	std::map<cclient::data::security::AuthInfo*, org::apache::accumulov2::core::securityImpl::thrift::TCredentials> convertedMap_V2;

  std::map<cclient::data::security::AuthInfo*, org::apache::accumulo::core::security::thrift::TCredentials> convertedMap;
  std::mutex mutex;

  void v1_authenticate(cclient::data::security::AuthInfo *auth);

  std::map<std::string, std::string> v1_getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName);

  Scan *v1_singleScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request);

  Scan *v1_multiScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request);

  Scan *v1_beginScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request);



  org::apache::accumulo::core::security::thrift::TCredentials getOrSetCredentials(cclient::data::security::AuthInfo *convert);

  void v1_registerService(std::string instance, std::string clusterManagers);


  Scan *v1_continueScan(Scan *originalScan);

  void *v1_write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<std::shared_ptr<cclient::data::Mutation>>> *request);

  bool v1_dropUser(cclient::data::security::AuthInfo *auth, std::string user);

  bool v1_changeUserPassword(cclient::data::security::AuthInfo *auth, std::string user, std::string password);

  bool v1_createUser(cclient::data::security::AuthInfo *auth, std::string user, std::string password);

  std::map<std::string, std::string> v1_getTableConfiguration(cclient::data::security::AuthInfo *auth, std::string table);

  cclient::data::security::Authorizations *v1_getUserAuths(cclient::data::security::AuthInfo *auth, std::string user);

  void v1_changeUserAuths(cclient::data::security::AuthInfo *auth, std::string user, cclient::data::security::Authorizations *auths);

  void v1_splitTablet(cclient::data::security::AuthInfo *auth, std::shared_ptr<cclient::data::KeyExtent> extent, std::string split);

  void v1_close();

  /***
   *
   * v2
   */

  void v2_authenticate(cclient::data::security::AuthInfo *auth);

  Scan *v2_singleScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request);

  Scan *v2_multiScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request);

  Scan *v2_beginScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request);

  void v2_registerService(std::string instance, std::string clusterManagers);

  org::apache::accumulov2::core::securityImpl::thrift::TCredentials getOrSetCredentialsV2(cclient::data::security::AuthInfo *convert);

  std::map<std::string, std::string> v2_getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName);

  Scan *v2_continueScan(Scan *originalScan);

  void *v2_write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<std::shared_ptr<cclient::data::Mutation>>> *request);

  bool v2_dropUser(cclient::data::security::AuthInfo *auth, std::string user);

  bool v2_changeUserPassword(cclient::data::security::AuthInfo *auth, std::string user, std::string password);

  bool v2_createUser(cclient::data::security::AuthInfo *auth, std::string user, std::string password);

  std::map<std::string, std::string> v2_getTableConfiguration(cclient::data::security::AuthInfo *auth, std::string table);

  cclient::data::security::Authorizations *v2_getUserAuths(cclient::data::security::AuthInfo *auth, std::string user);

  void v2_changeUserAuths(cclient::data::security::AuthInfo *auth, std::string user, cclient::data::security::Authorizations *auths);

  void v2_splitTablet(cclient::data::security::AuthInfo *auth, std::shared_ptr<cclient::data::KeyExtent> extent, std::string split);

  void v2_close();

  uint8_t accumuloVersion;

  std::unique_ptr<org::apache::accumulo::core::client::impl::thrift::ClientServiceClient> client;
  std::unique_ptr<org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient> tserverClient;

  std::unique_ptr<org::apache::accumulov2::core::clientImpl::thrift::ClientServiceClient> client_V2;
  std::unique_ptr<org::apache::accumulov2::core::tabletserver::thrift::TabletClientServiceClient> tserverClient_V2;
 public:

  AccumuloServerFacade();

  Scan *multiScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request);

  Scan * singleScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request);

  Scan *beginScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request);

  Scan *continueScan(Scan *originalScan);

  void *write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<std::shared_ptr<cclient::data::Mutation>>> *request);

  bool dropUser(cclient::data::security::AuthInfo *auth, const std::string &user);

  bool changeUserPassword(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password);

  bool createUser(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password);

  std::map<std::string, std::string> getTableConfiguration(cclient::data::security::AuthInfo *auth, const std::string &table);

  cclient::data::security::Authorizations *getUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user);

  void changeUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user, cclient::data::security::Authorizations *auths);

  void splitTablet(cclient::data::security::AuthInfo *auth, const std::shared_ptr<cclient::data::KeyExtent> extent, std::string split);

  void registerService(std::string instance, std::string clusterManagers);

  void close();

  void initialize(std::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr, bool callRegistration=true);

  std::map<std::string, std::string> getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName);

  void authenticate(cclient::data::security::AuthInfo *auth);

};

}
