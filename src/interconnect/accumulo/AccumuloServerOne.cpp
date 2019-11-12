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
#include "interconnect/accumulo/AccumuloServerOne.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"

namespace interconnect {

AccumuloServerFacadeV1::AccumuloServerFacadeV1()
    : AccumuloServerFacade(ACCUMULO_ONE),logger(logging::LoggerFactory<AccumuloServerFacadeV1>::getLogger()) {
}

Scan *AccumuloServerFacadeV1::multiScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  return v1_multiScan(isRunning,request);
}

Scan *AccumuloServerFacadeV1::AccumuloServerFacadeV1::singleScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  return v1_singleScan(isRunning,request);
}
Scan *AccumuloServerFacadeV1::beginScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  return v1_beginScan(isRunning,request);
}

Scan *AccumuloServerFacadeV1::continueScan(Scan *originalScan) {
  return v1_continueScan(originalScan);
}

void *AccumuloServerFacadeV1::write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<std::shared_ptr<cclient::data::Mutation>>> *request) {
  return v1_write(auth, request);
}

bool AccumuloServerFacadeV1::dropUser(cclient::data::security::AuthInfo *auth, const std::string &user) {
  return v1_dropUser(auth, user);
}

bool AccumuloServerFacadeV1::changeUserPassword(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
  return v1_changeUserPassword(auth, user, password);
}
bool AccumuloServerFacadeV1::createUser(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
  return v1_createUser(auth, user, password);
}

std::map<std::string, std::string> AccumuloServerFacadeV1::getTableConfiguration(cclient::data::security::AuthInfo *auth, const std::string &table) {

  std::map<std::string, std::string> ret;

  ret = v1_getTableConfiguration(auth, table);

  return ret;
}

cclient::data::security::Authorizations *AccumuloServerFacadeV1::getUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user) {
  return v1_getUserAuths(auth, user);
}

void AccumuloServerFacadeV1::changeUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user, cclient::data::security::Authorizations *auths) {
  return v1_changeUserAuths(auth, user, auths);
}

void AccumuloServerFacadeV1::splitTablet(cclient::data::security::AuthInfo *auth, const std::shared_ptr<cclient::data::KeyExtent> extent, std::string split) {
  return v1_splitTablet(auth, extent, split);
}

void AccumuloServerFacadeV1::registerService(std::string instance, std::string clusterManagers) {

      client->getZooKeepers(clusterManagers);
      client->getInstanceId(instance);


}

void AccumuloServerFacadeV1::close() {
      v1_close();
}

void AccumuloServerFacadeV1::v1_close() {
  if (NULL != client) {
    client = NULL;
  }
  if (NULL != tserverClient) {
    tserverClient = NULL;
  }
}


void AccumuloServerFacadeV1::initialize(std::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr, bool callRegistration) {

	  std::string zk,cm;
	  client = std::make_unique<org::apache::accumulo::core::client::impl::thrift::ClientServiceClient>(protocolPtr);
    tserverClient = std::make_unique<org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient>(protocolPtr);
    registerService(zk,cm);
}


std::map<std::string, std::string> AccumuloServerFacadeV1::getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName) {
  std::map<std::string, std::string> ret;

  ret = v1_getNamespaceConfiguration(auth, nameSpaceName);

  return ret;
}

void AccumuloServerFacadeV1::authenticate(cclient::data::security::AuthInfo *auth) {
  v1_authenticate(auth);
}

void AccumuloServerFacadeV1::v1_authenticate(cclient::data::security::AuthInfo *auth) {

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

std::map<std::string, std::string> AccumuloServerFacadeV1::v1_getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName) {
  org::apache::accumulo::core::trace::thrift::TInfo tinfo;
  org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  std::map<std::string, std::string> ret;
  client->getNamespaceConfiguration(ret, tinfo, creds, nameSpaceName);

  return ret;
}

Scan *AccumuloServerFacadeV1::v1_singleScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  Scan *initialScan = new Scan(isRunning);

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

  if (!results.more || !initialScan->isClientRunning()) {
    tserverClient->closeScan(scanId, scan.scanID);
  }

  delete kvs;

  return initialScan;
}

Scan *AccumuloServerFacadeV1::v1_multiScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  Scan *initialScan = new Scan(isRunning);

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

  tserverClient->startMultiScan(scan, scanId, ThriftWrapper::convert(request->getCredentials()), ThriftWrapper::convert(request->getRangeIdentifiers()), ThriftWrapper::convert(request->getColumns()),
                                ThriftWrapper::convert(iters), iterOptions, request->getAuthorizations()->getAuthorizations(), true);

  org::apache::accumulo::core::data::thrift::MultiScanResult results = scan.result;

  std::vector<std::shared_ptr<cclient::data::KeyValue> > *kvs = ThriftWrapper::convert(results.results);

  initialScan->setHasMore(results.more);

  initialScan->setNextResults(kvs);

  if (!results.more) {
    tserverClient->closeMultiScan(scanId, scan.scanID);
  }

  delete kvs;

  return initialScan;
}
org::apache::accumulo::core::security::thrift::TCredentials AccumuloServerFacadeV1::getOrSetCredentials(cclient::data::security::AuthInfo *convert) {
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

void AccumuloServerFacadeV1::v1_registerService(std::string instance, std::string clusterManagers) {
  client->getZooKeepers(clusterManagers);
  client->getInstanceId(instance);
}

Scan *AccumuloServerFacadeV1::v1_beginScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  Scan *initialScan = NULL;
  if (request->getRangeIdentifiers()->size() > 1) {
    initialScan = multiScan(isRunning,request);
  } else {
    ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> *ident = request->getRangeIdentifiers()->at(0);
    std::shared_ptr<cclient::data::KeyExtent> extent = ident->getGlobalMapping().at(0);
    cclient::data::Range *range = ident->getIdentifiers(extent).at(0);
    if (range->getStartKey() == NULL && range->getStopKey() == NULL) {
      initialScan = v1_multiScan(isRunning,request);
    } else
      initialScan = v1_singleScan(isRunning,request);

  }
  return initialScan;
}

interconnect::Scan *AccumuloServerFacadeV1::v1_continueScan(Scan *originalScan) {
  org::apache::accumulo::core::data::thrift::ScanResult results;
  org::apache::accumulo::core::trace::thrift::TInfo tinfo;

  org::apache::accumulo::core::data::thrift::ScanID scanId = originalScan->getId();

  tinfo.traceId = originalScan->getId() + 1;
  tinfo.parentId = originalScan->getId();
  try {
    tserverClient->continueScan(results, tinfo, scanId);

    std::vector<std::shared_ptr<cclient::data::KeyValue> > *kvs = ThriftWrapper::convert(results.results);

    if (results.more)
      originalScan->setTopKey(kvs->back()->getKey());

    originalScan->setHasMore(results.more);

    originalScan->setNextResults(kvs);

    if (!results.more || !originalScan->isClientRunning()) {
      tinfo.traceId++;
      tserverClient->closeScan(tinfo, originalScan->getId());
      results.more=false;
    }

    delete kvs;
  } catch (org::apache::accumulo::core::tabletserver::thrift::NotServingTabletException &te) {
    throw cclient::exceptions::NotServingException(te.what());
  }
  return originalScan;
}

void *AccumuloServerFacadeV1::v1_write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<std::shared_ptr<cclient::data::Mutation>>> *request) {

  org::apache::accumulo::core::trace::thrift::TInfo tinfo;
  org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  org::apache::accumulo::core::data::thrift::UpdateID upId = tserverClient->startUpdate(tinfo, creds, org::apache::accumulo::core::tabletserver::thrift::TDurability::DEFAULT);
  for (auto it = request->begin(); it != request->end(); it++) {

    tserverClient->applyUpdates(tinfo, upId, ThriftWrapper::convert(it->first), ThriftWrapper::convert(&it->second));
  }
  tinfo.parentId = tinfo.traceId;
  tinfo.traceId = tinfo.traceId + 1;
  org::apache::accumulo::core::data::thrift::UpdateErrors errors;
  tserverClient->closeUpdate(errors, tinfo, upId);
//@TODO return errors
  return 0;
}

bool AccumuloServerFacadeV1::v1_dropUser(cclient::data::security::AuthInfo *auth, std::string user) {
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

bool AccumuloServerFacadeV1::v1_changeUserPassword(cclient::data::security::AuthInfo *auth, std::string user, std::string password) {
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
bool AccumuloServerFacadeV1::v1_createUser(cclient::data::security::AuthInfo *auth, std::string user, std::string password) {
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

std::map<std::string, std::string> AccumuloServerFacadeV1::v1_getTableConfiguration(cclient::data::security::AuthInfo *auth, std::string table) {
  org::apache::accumulo::core::trace::thrift::TInfo tinfo;
  org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  std::map<std::string, std::string> ret;
  client->getTableConfiguration(ret, tinfo, creds, table);

  return ret;
}

cclient::data::security::Authorizations *AccumuloServerFacadeV1::v1_getUserAuths(cclient::data::security::AuthInfo *auth, std::string user) {
  org::apache::accumulo::core::trace::thrift::TInfo tinfo;
  org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  std::vector<std::string> returnStrings;
  client->getUserAuthorizations(returnStrings, tinfo, creds, user);
  return new cclient::data::security::Authorizations(&returnStrings);
}

void AccumuloServerFacadeV1::v1_changeUserAuths(cclient::data::security::AuthInfo *auth, std::string user, cclient::data::security::Authorizations *auths) {
  org::apache::accumulo::core::trace::thrift::TInfo tinfo;
  org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  client->changeAuthorizations(tinfo, creds, user, auths->getAuthorizations());
}

void AccumuloServerFacadeV1::v1_splitTablet(cclient::data::security::AuthInfo *auth, std::shared_ptr<cclient::data::KeyExtent> extent, std::string split) {
  org::apache::accumulo::core::trace::thrift::TInfo tinfo;
  org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

  org::apache::accumulo::core::data::thrift::TKeyExtent ke = ThriftWrapper::convert(extent);
  tinfo.parentId = 0;
  tinfo.traceId = rand();
  tserverClient->splitTablet(tinfo, creds, ke, split);
}

} /* namespace interconnect */
