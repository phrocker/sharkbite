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
#include "interconnect/accumulo/AccumuloServerTwo.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"

namespace interconnect {

AccumuloServerFacadeV2::AccumuloServerFacadeV2()
    : AccumuloServerFacade(ACCUMULO_TWO),
      logger(logging::LoggerFactory<AccumuloServerFacadeV2>::getLogger()) {
}

Scan *AccumuloServerFacadeV2::multiScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  return v2_multiScan(isRunning,request);
}

Scan *AccumuloServerFacadeV2::AccumuloServerFacadeV2::singleScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  return v2_singleScan(isRunning,request);
}
Scan *AccumuloServerFacadeV2::beginScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  return v2_beginScan(isRunning,request);
}

Scan *AccumuloServerFacadeV2::continueScan(Scan *originalScan) {
  return v2_continueScan(originalScan);
}

void *AccumuloServerFacadeV2::write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<std::shared_ptr<cclient::data::Mutation>>> *request) {
  return v2_write(auth, request);

}

bool AccumuloServerFacadeV2::dropUser(cclient::data::security::AuthInfo *auth, const std::string &user) {
  return v2_dropUser(auth, user);
}

bool AccumuloServerFacadeV2::changeUserPassword(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
  return v2_changeUserPassword(auth, user, password);

}
bool AccumuloServerFacadeV2::createUser(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
  return v2_createUser(auth, user, password);
}

std::map<std::string, std::string> AccumuloServerFacadeV2::getTableConfiguration(cclient::data::security::AuthInfo *auth, const std::string &table) {

  std::map<std::string, std::string> ret;

  ret = v2_getTableConfiguration(auth, table);

  return ret;
}

cclient::data::security::Authorizations *AccumuloServerFacadeV2::getUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user) {
  return v2_getUserAuths(auth, user);
}

void AccumuloServerFacadeV2::changeUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user, cclient::data::security::Authorizations *auths) {
  return v2_changeUserAuths(auth, user, auths);
}

void AccumuloServerFacadeV2::splitTablet(cclient::data::security::AuthInfo *auth, const std::shared_ptr<cclient::data::KeyExtent> extent, std::string split) {
  return v2_splitTablet(auth, extent, split);
}

void AccumuloServerFacadeV2::registerService(std::string instance, std::string clusterManagers) {

  client_V2->getInstanceId(instance);
  client_V2->getZooKeepers(clusterManagers);
}

void AccumuloServerFacadeV2::close() {
  v2_close();
}

void AccumuloServerFacadeV2::initialize(std::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr, bool callRegistration) {

  std::string zk, cm;
  client_V2 = std::make_unique<org::apache::accumulov2::core::clientImpl::thrift::ClientServiceClient>(protocolPtr);
  tserverClient_V2 = std::make_unique<org::apache::accumulov2::core::tabletserver::thrift::TabletClientServiceClient>(protocolPtr);
  accumuloVersion = ACCUMULO_TWO;
}

std::map<std::string, std::string> AccumuloServerFacadeV2::getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName) {
  std::map<std::string, std::string> ret;

  ret = v2_getNamespaceConfiguration(auth, nameSpaceName);
  return ret;
}

void AccumuloServerFacadeV2::authenticate(cclient::data::security::AuthInfo *auth) {
  v2_authenticate(auth);
}

void AccumuloServerFacadeV2::v2_authenticate(cclient::data::security::AuthInfo *auth) {

  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();

  try {
    if (!client_V2->authenticateUser(tinfo, creds, creds)) {
      throw cclient::exceptions::ClientException("Invalid username");
    }
  } catch (const org::apache::accumulov2::core::clientImpl::thrift::ThriftSecurityException &tse) {
    throw cclient::exceptions::ClientException(INVALID_USERNAME_PASSWORD);
  }
}

std::map<std::string, std::string> AccumuloServerFacadeV2::v2_getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName) {
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  std::map<std::string, std::string> ret;
  client_V2->getNamespaceConfiguration(ret, tinfo, creds, nameSpaceName);

  return ret;
}

Scan * AccumuloServerFacadeV2::v2_singleScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  Scan *initialScan = new Scan(isRunning);

  org::apache::accumulov2::core::dataImpl::thrift::InitialScan scan;

  org::apache::accumulov2::core::trace::thrift::TInfo scanId;

  scanId.parentId = 0;
  scanId.traceId = rand();

  const std::vector<cclient::data::IterInfo> &iters = request->getIterators();
  std::map<std::string, std::map<std::string, std::string> > iterOptions;
  for (auto it = iters.begin(); it != iters.end(); it++) {
    auto myOptions = (*it).getOptions();
    for (auto optIt = myOptions.begin(); optIt != myOptions.end(); optIt++) {
      iterOptions[(*it).getName()][(*optIt).first] = (*optIt).second;
    }
  }

  ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> *ident = request->getRangeIdentifiers()->at(0);
  std::shared_ptr<cclient::data::KeyExtent> extent = ident->getGlobalMapping().at(0);
  cclient::data::Range *range = ident->getIdentifiers(extent).at(0);
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = getOrSetCredentialsV2(request->getCredentials());
  /**
   * void startScan( ::org::apache::accumulov2::core::dataImpl::thrift::InitialScan& _return,
   * const  ::org::apache::accumulov2::core::trace::thrift::TInfo& tinfo,
   * const  ::org::apache::accumulov2::core::securityImpl::thrift::TCredentials& credentials,
   * const  ::org::apache::accumulov2::core::dataImpl::thrift::TKeyExtent& extent,
   * const  ::org::apache::accumulov2::core::dataImpl::thrift::TRange& range,
   * const std::vector< ::org::apache::accumulov2::core::dataImpl::thrift::TColumn> & columns,
   * const int32_t batchSize, const std::vector< ::org::apache::accumulov2::core::dataImpl::thrift::IterInfo> & ssiList,
   * const std::map<std::string, std::map<std::string, std::string> > & ssio, const std::vector<std::string> & authorizations,
   * const bool waitForWrites, const bool isolated, const int64_t readaheadThreshold,
   *
   *
   * const TSamplerConfiguration& samplerConfig, const int64_t batchTimeOut,
   *  const std::string& classLoaderContext, const std::map<std::string, std::string> & executionHints);
   */
  org::apache::accumulov2::core::tabletserver::thrift::TSamplerConfiguration config;
  std::map<std::string, std::string> executionHints;


  logging::LOG_DEBUG(logger) << "extent is " << extent << " and range is " << range;

  try{
    tserverClient_V2->startScan(scan, scanId, creds, ThriftV2Wrapper::convert(extent), ThriftV2Wrapper::convert(range), ThriftV2Wrapper::convert(request->getColumns()), 1024,
                              ThriftV2Wrapper::convert(iters), iterOptions, request->getAuthorizations()->getAuthorizations(), true, false, 1024, config, 1024 * 5, "", executionHints);
  } catch (const apache::thrift::TApplicationException &te) {
    logging::LOG_DEBUG(logger) << "Error on extent" << extent << " and range is " << range;
    throw te;
  }



  org::apache::accumulov2::core::dataImpl::thrift::ScanResult results = scan.result;

  logging::LOG_DEBUG(logger) << "extent is " << extent << " columns " << request->getColumns().size() << " has more? " << (results.more);

  std::vector<std::shared_ptr<cclient::data::KeyValue> > *kvs = ThriftV2Wrapper::convert(results.results);

  initialScan->setHasMore(results.more);

  initialScan->setScanId(scan.scanID);

  initialScan->setNextResults(kvs);

  if (!results.more) {
    tserverClient_V2->closeScan(scanId, scan.scanID);
  }

  delete kvs;

  return initialScan;
}

Scan * AccumuloServerFacadeV2::v2_multiScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  Scan *initialScan = new Scan(isRunning);

  initialScan->setMultiScan(true);

  org::apache::accumulov2::core::dataImpl::thrift::InitialMultiScan scan;

  org::apache::accumulov2::core::trace::thrift::TInfo scanId;

  scanId.traceId = rand();
  scanId.parentId = scan.scanID;

  const std::vector<cclient::data::IterInfo> &iters = request->getIterators();
  std::map<std::string, std::map<std::string, std::string> > iterOptions;
  for (auto it = iters.begin(); it != iters.end(); it++) {
    auto myOptions = (*it).getOptions();
    for (auto optIt = myOptions.begin(); optIt != myOptions.end(); optIt++) {
      iterOptions[(*it).getName()][(*optIt).first] = (*optIt).second;
    }
  }

  /**
   * void startMultiScan( ::org::apache::accumulov2::core::dataImpl::thrift::InitialMultiScan& _return,
   * const  ::org::apache::accumulov2::core::trace::thrift::TInfo& tinfo,
   * const  ::org::apache::accumulov2::core::securityImpl::thrift::TCredentials& credentials,
   * const  ::org::apache::accumulov2::core::dataImpl::thrift::ScanBatch& batch,
   * const std::vector< ::org::apache::accumulov2::core::dataImpl::thrift::TColumn> & columns,
   * const std::vector< ::org::apache::accumulov2::core::dataImpl::thrift::IterInfo> & ssiList,
   * const std::map<std::string, std::map<std::string, std::string> > & ssio,
   const std::vector<std::string> & authorizations, const bool waitForWrites,
   const TSamplerConfiguration& samplerConfig, const int64_t batchTimeOut,
   const std::string& classLoaderContext, const std::map<std::string, std::string> & executionHints);
   */

  org::apache::accumulov2::core::tabletserver::thrift::TSamplerConfiguration config;
  std::map<std::string, std::string> executionHints;

  logging::LOG_DEBUG(logger) << "multiscan extent is scan id " << scanId;

  try{
  tserverClient_V2->startMultiScan(scan, scanId, ThriftV2Wrapper::convert(request->getCredentials()), ThriftV2Wrapper::convert(request->getRangeIdentifiers()),
                                   ThriftV2Wrapper::convert(request->getColumns()), ThriftV2Wrapper::convert(iters), iterOptions, request->getAuthorizations()->getAuthorizations(), true, config,
                                   1024 * 5, "", executionHints);

  } catch (const apache::thrift::TApplicationException &te) {
      auto batch =ThriftV2Wrapper::convert(request->getRangeIdentifiers());
      for(auto d : batch){
        std::stringstream st;
        d.first.printTo(st);
        for(auto rng : d.second){
          std::stringstream str;
          rng.printTo(str);

          logging::LOG_DEBUG(logger) << "failed scan on extent is " << st.str() << " range is " << str.str();
        }
      }
      throw te;
    }

  org::apache::accumulov2::core::dataImpl::thrift::MultiScanResult results = scan.result;

  std::vector<std::shared_ptr<cclient::data::KeyValue> > *kvs = ThriftV2Wrapper::convert(results.results);

  initialScan->setHasMore(results.more);

  initialScan->setNextResults(kvs);

  initialScan->setScanId(scan.scanID);

  logging::LOG_DEBUG(logger) << "multiscan return " << scan.scanID << " result set size is " << ( kvs != nullptr ? kvs->size() : 0 );

  if (!results.more) {
    tserverClient_V2->closeMultiScan(scanId, scan.scanID);
  }

  delete kvs;

  return initialScan;
}
org::apache::accumulov2::core::securityImpl::thrift::TCredentials AccumuloServerFacadeV2::getOrSetCredentialsV2(cclient::data::security::AuthInfo * convert) {
  std::lock_guard<std::mutex> lock(mutex);
  std::map<cclient::data::security::AuthInfo*, org::apache::accumulov2::core::securityImpl::thrift::TCredentials>::iterator it;
  it = convertedMap_V2.find(convert);
  if (it != convertedMap_V2.end()) {
    return it->second;
  }

  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(convert);
  convertedMap_V2.insert(std::pair<cclient::data::security::AuthInfo*, org::apache::accumulov2::core::securityImpl::thrift::TCredentials>(convert, creds));
  return creds;

}

void AccumuloServerFacadeV2::v2_registerService(std::string instance, std::string clusterManagers) {
  client_V2->getZooKeepers(clusterManagers);
  client_V2->getInstanceId(instance);
}

Scan * AccumuloServerFacadeV2::v2_beginScan(std::atomic<bool> *isRunning,ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
  Scan *initialScan = NULL;
  size_t size = 0;
  for(auto sz : *request->getRangeIdentifiers()){
    size += sz->size();
  }
  if (size > 1) {
    initialScan = multiScan(isRunning,request);
  } else {
    ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> *ident = request->getRangeIdentifiers()->at(0);
    std::shared_ptr<cclient::data::KeyExtent> extent = ident->getGlobalMapping().at(0);
    cclient::data::Range *range = ident->getIdentifiers(extent).at(0);
    if (range->getStartKey() == NULL && range->getStopKey() == NULL) {
      initialScan = v2_multiScan(isRunning,request);
    } else
      initialScan = v2_singleScan(isRunning,request);

  }
  return initialScan;
}

Scan * AccumuloServerFacadeV2::v2_continueMultiScan(Scan * originalScan) {

  org::apache::accumulov2::core::dataImpl::thrift::MultiScanResult results;
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;

  org::apache::accumulov2::core::dataImpl::thrift::ScanID scanId = originalScan->getId();

  tinfo.traceId = originalScan->getId() + 1;
  tinfo.parentId = originalScan->getId();
  try {

      tserverClient_V2->continueMultiScan(results, tinfo, scanId);


    std::vector<std::shared_ptr<cclient::data::KeyValue> > *kvs = ThriftV2Wrapper::convert(results.results);

    if (results.more && !kvs->empty())
      originalScan->setTopKey(kvs->back()->getKey());

    originalScan->setHasMore(results.more);

    originalScan->setNextResults(kvs);
    if (!results.more || !originalScan->isClientRunning()) {
      tinfo.traceId++;
      tserverClient_V2->closeScan(tinfo, originalScan->getId());
      results.more=false;
    }

    delete kvs;
  } catch (org::apache::accumulov2::core::tabletserver::thrift::NotServingTabletException &te) {
    throw cclient::exceptions::NotServingException(te.what());
  } catch (const org::apache::accumulov2::core::tabletserver::thrift::NoSuchScanIDException &te) {
    logging::LOG_DEBUG(logger) << "Continue Scan halted. No Such Scan ID, so setting no more results";
    originalScan->setHasMore(false);
  }
  return originalScan;
}

Scan * AccumuloServerFacadeV2::v2_continueScan(Scan * originalScan) {
  if (originalScan->isMultiScan()){
      return v2_continueMultiScan(originalScan);
    }
  org::apache::accumulov2::core::dataImpl::thrift::ScanResult results;
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;

  org::apache::accumulov2::core::dataImpl::thrift::ScanID scanId = originalScan->getId();

  tinfo.traceId = originalScan->getId() + 1;
  tinfo.parentId = originalScan->getId();
  try {
    tserverClient_V2->continueScan(results, tinfo, scanId);

    std::vector<std::shared_ptr<cclient::data::KeyValue> > *kvs = ThriftV2Wrapper::convert(results.results);

    if (results.more && !kvs->empty())
      originalScan->setTopKey(kvs->back()->getKey());

    originalScan->setHasMore(results.more);

    originalScan->setNextResults(kvs);
    if (!results.more || !originalScan->isClientRunning()) {
      tinfo.traceId++;
      tserverClient_V2->closeScan(tinfo, originalScan->getId());
      results.more=false;
    }

    delete kvs;
  } catch (org::apache::accumulov2::core::tabletserver::thrift::NotServingTabletException &te) {
    throw cclient::exceptions::NotServingException(te.what());
  } catch (const org::apache::accumulov2::core::tabletserver::thrift::NoSuchScanIDException &te) {
    logging::LOG_DEBUG(logger) << "Continue Scan halted. No Such Scan ID, so setting no more results";
    originalScan->setHasMore(false);
  }
  return originalScan;
}

void *AccumuloServerFacadeV2::v2_write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<std::shared_ptr<cclient::data::Mutation>>> *request) {

  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  org::apache::accumulov2::core::dataImpl::thrift::UpdateID upId = tserverClient_V2->startUpdate(tinfo, creds, org::apache::accumulov2::core::tabletserver::thrift::TDurability::DEFAULT);
  for (auto it = request->begin(); it != request->end(); it++) {

    tserverClient_V2->applyUpdates(tinfo, upId, ThriftV2Wrapper::convert(it->first), ThriftV2Wrapper::convert(&it->second));
  }
  tinfo.parentId = tinfo.traceId;
  tinfo.traceId = tinfo.traceId + 1;
  org::apache::accumulov2::core::dataImpl::thrift::UpdateErrors errors;
  tserverClient_V2->closeUpdate(errors, tinfo, upId);
//@TODO return errors
  return 0;
}

bool AccumuloServerFacadeV2::v2_dropUser(cclient::data::security::AuthInfo *auth, std::string user) {
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  try {
    client_V2->dropLocalUser(tinfo, creds, user);
    return true;
  } catch (const org::apache::accumulov2::core::clientImpl::thrift::ThriftSecurityException &tse) {
    // could not create the user for some reason
    return false;
  }
}

bool AccumuloServerFacadeV2::v2_changeUserPassword(cclient::data::security::AuthInfo *auth, std::string user, std::string password) {
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();

  try {
    client_V2->changeLocalUserPassword(tinfo, creds, user, password);
    return true;
  } catch (const org::apache::accumulov2::core::clientImpl::thrift::ThriftSecurityException &tse) {
    // could not create the user for some reason
    return false;

  }
}
bool AccumuloServerFacadeV2::v2_createUser(cclient::data::security::AuthInfo *auth, std::string user, std::string password) {
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  try {
    client_V2->createLocalUser(tinfo, creds, user, password);
    return true;
  } catch (const org::apache::accumulov2::core::clientImpl::thrift::ThriftSecurityException &tse) {
    // could not create the user for some reason
    return false;

  }

}

std::map<std::string, std::string> AccumuloServerFacadeV2::v2_getTableConfiguration(cclient::data::security::AuthInfo *auth, std::string table) {
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  std::map<std::string, std::string> ret;
  client_V2->getTableConfiguration(ret, tinfo, creds, table);

  return ret;
}

cclient::data::security::Authorizations *AccumuloServerFacadeV2::v2_getUserAuths(cclient::data::security::AuthInfo *auth, std::string user) {
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  std::vector<std::string> returnStrings;
  client_V2->getUserAuthorizations(returnStrings, tinfo, creds, user);
  return new cclient::data::security::Authorizations(&returnStrings);
}

void AccumuloServerFacadeV2::v2_changeUserAuths(cclient::data::security::AuthInfo *auth, std::string user, cclient::data::security::Authorizations *auths) {
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  tinfo.parentId = 0;
  tinfo.traceId = rand();
  client_V2->changeAuthorizations(tinfo, creds, user, auths->getAuthorizations());
}

void AccumuloServerFacadeV2::v2_splitTablet(cclient::data::security::AuthInfo *auth, std::shared_ptr<cclient::data::KeyExtent> extent, std::string split) {
  org::apache::accumulov2::core::trace::thrift::TInfo tinfo;
  org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);

  org::apache::accumulov2::core::dataImpl::thrift::TKeyExtent ke = ThriftV2Wrapper::convert(extent);
  tinfo.parentId = 0;
  tinfo.traceId = rand();
  tserverClient_V2->splitTablet(tinfo, creds, ke, split);
}

void AccumuloServerFacadeV2::v2_close() {

  if (NULL != client_V2) {
    client_V2 = NULL;
  }
  if (NULL != tserverClient_V2) {
    tserverClient_V2 = NULL;
  }
}
} /* namespace interconnect */
