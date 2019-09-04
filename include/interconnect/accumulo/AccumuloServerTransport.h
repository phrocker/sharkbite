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

#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/TabletClientService.h"
#include <mutex>

#include <boost/concept_check.hpp>
#include <boost/shared_ptr.hpp>
#include "data/extern/boost/SharedPointer.h"
#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/TabletClientService.h"
#include "../scanrequest/ScanRequest.h"
#include "../scanrequest/ScanIdentifier.h"
#include "data/extern/thrift/ThriftWrapper.h"
#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"
#include "interconnect/accumulo/AccumuloServerTransport.h"

namespace interconnect {

#define ACCUMULO_UNKNOWN 0

#define ACCUMULO_ONE 1

#define ACCUMULO_TWO 2

class AccumuloServerTransport {

 private:

  std::map<cclient::data::security::AuthInfo*, org::apache::accumulo::core::security::thrift::TCredentials> convertedMap;
  std::mutex mutex;

  void v1_authenticate(cclient::data::security::AuthInfo *auth) {

    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();

    try {
      if (!client->authenticateUser(tinfo, creds, creds)) {
        throw cclient::exceptions::ClientException("Invalid username");
      }
    } catch (const org::apache::accumulo::core::client::impl::thrift::ThriftSecurityException &tse) {
      throw cclient::exceptions::ClientException(INVALID_USERNAME_PASSWORD);
    }
  }

  std::map<std::string, std::string> v1_getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName) {
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();
    std::map<std::string, std::string> ret;
    client->getNamespaceConfiguration(ret, tinfo, creds, nameSpaceName);

    return ret;
  }

  void v1_close() {
    if (NULL != client) {
      //      delete client;
      client = NULL;
    }
    if (NULL != tserverClient) {
      tserverClient = NULL;
    }
  }

  Scan *
  v1_singleScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    Scan *initialScan = new Scan();

    org::apache::accumulo::core::data::thrift::InitialScan scan;

    org::apache::accumulo::core::trace::thrift::TInfo scanId;

    scanId.parentId = 0;
    scanId.traceId = rand();

    std::vector<cclient::data::IterInfo*> *iters = request->getIterators();
    std::map<std::string, std::map<std::string, std::string> > iterOptions;
    for (auto it = iters->begin(); it != iters->end(); it++) {
      auto myOptions = (*it)->getOptions();
      for (auto optIt = myOptions.begin(); optIt != myOptions.end(); optIt++) {
        iterOptions[(*it)->getName()][(*optIt).first] = (*optIt).second;
      }
    }

    ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> *ident = request->getRangeIdentifiers()->at(0);
    std::shared_ptr<cclient::data::KeyExtent> extent = ident->getGlobalMapping().at(0);
    cclient::data::Range *range = ident->getIdentifiers(extent).at(0);
    org::apache::accumulo::core::security::thrift::TCredentials creds = getOrSetCredentials(request->getCredentials());
    tserverClient->startScan(scan, scanId, creds, ThriftWrapper::convert(extent), ThriftWrapper::convert(range), ThriftWrapper::convert(request->getColumns()), 1024, ThriftWrapper::convert(iters),
                             iterOptions, request->getAuthorizations()->getAuthorizations(), true, false, 1024);

    org::apache::accumulo::core::data::thrift::ScanResult results = scan.result;

    std::vector<std::shared_ptr<cclient::data::KeyValue> > *kvs = ThriftWrapper::convert(results.results);

    initialScan->setHasMore(results.more);

    initialScan->setScanId(scan.scanID);

    initialScan->setNextResults(kvs);

    if (!results.more) {
      tserverClient->closeScan(scanId, scan.scanID);
    }

    delete kvs;

    return initialScan;
  }

  Scan *
  v1_multiScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    Scan *initialScan = new Scan();

    org::apache::accumulo::core::data::thrift::InitialMultiScan scan;

    org::apache::accumulo::core::trace::thrift::TInfo scanId;

    scanId.traceId = scan.scanID;
    scanId.parentId = scan.scanID;

    std::vector<cclient::data::IterInfo*> *iters = request->getIterators();
    std::map<std::string, std::map<std::string, std::string> > iterOptions;
    for (auto it = iters->begin(); it != iters->end(); it++) {
      auto myOptions = (*it)->getOptions();
      for (auto optIt = myOptions.begin(); optIt != myOptions.end(); optIt++) {
        iterOptions[(*it)->getName()][(*optIt).first] = (*optIt).second;
      }
    }

    tserverClient->startMultiScan(scan, scanId, ThriftWrapper::convert(request->getCredentials()), ThriftWrapper::convert(request->getRangeIdentifiers()),
                                  ThriftWrapper::convert(request->getColumns()), ThriftWrapper::convert(iters), iterOptions, request->getAuthorizations()->getAuthorizations(), true);

    org::apache::accumulo::core::data::thrift::MultiScanResult results = scan.result;

    std::vector<std::shared_ptr<cclient::data::KeyValue> > *kvs = ThriftWrapper::convert(results.results);

    initialScan->setHasMore(results.more);

    initialScan->setNextResults(kvs);

    tserverClient->closeMultiScan(scanId, scan.scanID);

    delete kvs;

    return initialScan;
  }
  org::apache::accumulo::core::security::thrift::TCredentials getOrSetCredentials(cclient::data::security::AuthInfo *convert) {
    std::lock_guard<std::mutex> lock(mutex);
    std::map<cclient::data::security::AuthInfo*, org::apache::accumulo::core::security::thrift::TCredentials>::iterator it;
    it = convertedMap.find(convert);
    if (it != convertedMap.end()) {
      return it->second;
    }

    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(convert);
    convertedMap.insert(std::pair<cclient::data::security::AuthInfo*, org::apache::accumulo::core::security::thrift::TCredentials>(convert, creds));
    return creds;

  }

  void v1_registerService(std::string instance, std::string clusterManagers) {

    /*
     boost::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr(new apache::thrift::protocol::TCompactProtocol(underlyingTransport));

     if (NULL != client) {
     //delete client;
     client = NULL;
     }
     if (NULL != tserverClient) {
     tserverClient = NULL;
     }

     client = new org::apache::accumulo::core::client::impl::thrift::ClientServiceClient(protocolPtr);
     tserverClient = std::unique_ptr < org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient
     > (new org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient(protocolPtr));
     */
    client->getZooKeepers(clusterManagers);
    client->getInstanceId(instance);
  }

  Scan *
  v1_beginScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    Scan *initialScan = NULL;
    if (request->getRangeIdentifiers()->size() > 1) {
      initialScan = multiScan(request);
    } else {
      ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> *ident = request->getRangeIdentifiers()->at(0);
      std::shared_ptr<cclient::data::KeyExtent> extent = ident->getGlobalMapping().at(0);
      cclient::data::Range *range = ident->getIdentifiers(extent).at(0);
      if (range->getStartKey() == NULL && range->getStopKey() == NULL) {
        initialScan = v1_multiScan(request);
      } else
        initialScan = v1_singleScan(request);

    }
    return initialScan;
  }

  Scan *v1_continueScan(Scan *originalScan) {
    org::apache::accumulo::core::data::thrift::ScanResult results;
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;

    org::apache::accumulo::core::data::thrift::ScanID scanId = originalScan->getId();

    tinfo.traceId = originalScan->getId() + 1;
    tinfo.parentId = originalScan->getId();
    tserverClient->continueScan(results, tinfo, scanId);

    std::vector<std::shared_ptr<cclient::data::KeyValue> > *kvs = ThriftWrapper::convert(results.results);

    if (results.more)
      originalScan->setTopKey(kvs->back()->getKey());

    originalScan->setHasMore(results.more);

    originalScan->setNextResults(kvs);

    if (!results.more) {
      tinfo.traceId++;
      tserverClient->closeScan(tinfo, originalScan->getId());
    }

    delete kvs;

    return originalScan;
  }

  void *v1_write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<cclient::data::Mutation*>> *request) {

    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();
    org::apache::accumulo::core::data::thrift::UpdateID upId = tserverClient->startUpdate(tinfo, creds, org::apache::accumulo::core::tabletserver::thrift::TDurability::DEFAULT);
    for (std::map<cclient::data::KeyExtent, std::vector<cclient::data::Mutation*>>::iterator it = request->begin(); it != request->end(); it++) {

      tserverClient->applyUpdates(tinfo, upId, ThriftWrapper::convert(it->first), ThriftWrapper::convert(&it->second));
    }
    tinfo.parentId = tinfo.traceId;
    tinfo.traceId = tinfo.traceId + 1;
    org::apache::accumulo::core::data::thrift::UpdateErrors errors;
    tserverClient->closeUpdate(errors, tinfo, upId);
//@TODO return errors
    return 0;
  }

  bool v1_dropUser(cclient::data::security::AuthInfo *auth, std::string user) {
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();
    try {
      client->dropLocalUser(tinfo, creds, user);
      return true;
    } catch (const org::apache::accumulo::core::client::impl::thrift::ThriftSecurityException &tse) {
      // could not create the user for some reason
      return false;
    }
  }

  bool v1_changeUserPassword(cclient::data::security::AuthInfo *auth, std::string user, std::string password) {
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();

    try {
      client->changeLocalUserPassword(tinfo, creds, user, password);
      return true;
    } catch (const org::apache::accumulo::core::client::impl::thrift::ThriftSecurityException &tse) {
      // could not create the user for some reason
      return false;

    }
  }
  bool v1_createUser(cclient::data::security::AuthInfo *auth, std::string user, std::string password) {
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();
    try {
      client->createLocalUser(tinfo, creds, user, password);
      return true;
    } catch (const org::apache::accumulo::core::client::impl::thrift::ThriftSecurityException &tse) {
      // could not create the user for some reason
      return false;

    }

  }

  std::map<std::string, std::string> v1_getTableConfiguration(cclient::data::security::AuthInfo *auth, std::string table) {
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();
    std::map<std::string, std::string> ret;
    client->getTableConfiguration(ret, tinfo, creds, table);

    return ret;
  }

  cclient::data::security::Authorizations *v1_getUserAuths(cclient::data::security::AuthInfo *auth, std::string user) {
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();
    std::vector<std::string> returnStrings;
    client->getUserAuthorizations(returnStrings, tinfo, creds, user);
    return new cclient::data::security::Authorizations(&returnStrings);
  }

  void v1_changeUserAuths(cclient::data::security::AuthInfo *auth, std::string user, cclient::data::security::Authorizations *auths) {
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();
    client->changeAuthorizations(tinfo, creds, user, auths->getAuthorizations());
  }

  void v1_splitTablet(cclient::data::security::AuthInfo *auth, std::shared_ptr<cclient::data::KeyExtent> extent, std::string split) {
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    org::apache::accumulo::core::data::thrift::TKeyExtent ke = ThriftWrapper::convert(extent);
    tinfo.parentId = 0;
    tinfo.traceId = rand();
    tserverClient->splitTablet(tinfo, creds, ke, split);
  }

  uint8_t accumuloVersion;

  std::unique_ptr<org::apache::accumulo::core::client::impl::thrift::ClientServiceClient> client;
  std::unique_ptr<org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient> tserverClient;
 public:

  Scan *multiScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_multiScan(request);
    };
    return nullptr;
  }

  Scan * singleScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_singleScan(request);
    };
    return nullptr;
  }
  Scan *
  beginScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_beginScan(request);
    };
    return nullptr;
  }

  Scan *continueScan(Scan *originalScan) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_continueScan(originalScan);
    };
    return nullptr;
  }

  void *write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<cclient::data::Mutation*>> *request) {

    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_write(auth, request);
    };
    return nullptr;
  }

  bool dropUser(cclient::data::security::AuthInfo *auth, const std::string &user) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_dropUser(auth, user);

    };
    return false;
  }

  bool changeUserPassword(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_changeUserPassword(auth, user, password);
    };
    return false;
  }
  bool createUser(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_createUser(auth, user, password);
    };
    return false;
  }

  std::map<std::string, std::string> getTableConfiguration(cclient::data::security::AuthInfo *auth, const std::string &table) {

    std::map<std::string, std::string> ret;

    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        ret = v1_getTableConfiguration(auth, table);
        break;
    };

    return ret;
  }

  cclient::data::security::Authorizations *getUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_getUserAuths(auth, user);
    };
    return nullptr;
  }

  void changeUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user, cclient::data::security::Authorizations *auths) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_changeUserAuths(auth, user, auths);
        break;
    };
  }

  void splitTablet(cclient::data::security::AuthInfo *auth, const std::shared_ptr<cclient::data::KeyExtent> extent, std::string split) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_splitTablet(auth, extent, split);
        break;
    };
  }

  void registerService(std::string instance, std::string clusterManagers) {

    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        client->getZooKeepers(clusterManagers);
        client->getInstanceId(instance);
        break;
    };

  }

  void close() {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        v1_close();
        break;
    };
  }

  void initialize(boost::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr) {
    client = std::make_unique<org::apache::accumulo::core::client::impl::thrift::ClientServiceClient>(protocolPtr);
    tserverClient = std::make_unique<org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient>(protocolPtr);
    accumuloVersion = ACCUMULO_ONE;
  }

  std::map<std::string, std::string> getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName) {
    std::map<std::string, std::string> ret;

    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        ret = v1_getNamespaceConfiguration(auth, nameSpaceName);
        break;
    };

    return ret;
  }

  void authenticate(cclient::data::security::AuthInfo *auth) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        v1_authenticate(auth);
        break;
    };
  }

};

}
