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
#ifndef SRC_INTERCONNECT_TRANSPORT_ACCUMULOMASTERTRANSPOTER_H_
#define SRC_INTERCONNECT_TRANSPORT_ACCUMULOMASTERTRANSPOTER_H_

#include <concurrency/ThreadManager.h>

#include <chrono>
#include <thread>
#include <sys/time.h>
#include <locale>

#include <map>
#include <set>
#include <algorithm>    // std::random_shuffle
#include <vector>       // std::vector
#include <ctime>        // std::time
#include <cstdlib>      // std::rand, std::srand7

#include "data/constructs/inputvalidation.h"
#include "data/constructs/IterInfo.h"
#include "data/constructs/configuration/Configuration.h"
#include "data/extern/thrift/data_types.h"
#include "data/extern/thrift/tabletserver_types.h"
#include "data/constructs/scanstate.h"
#include "data/exceptions/ClientException.h"
#include "data/exceptions/IllegalArgumentException.h"
#include "data/constructs/tablet/TabletType.h"
#include "../scanrequest/ScanIdentifier.h"

#include "Transport.h"
#include <boost/concept_check.hpp>
#include <boost/shared_ptr.hpp>

#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/master_types.h"
#include "data/extern/thrift/MasterClientService.h"
#include "data/extern/thrift/ThriftWrapper.h"
#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"

#include "BaseTransport.h"
#include "FateInterface.h"
namespace interconnect {
//#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>

#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransportException.h>
#include <transport/TBufferTransports.h>

/**
 * Represents the interconnect to the master client
 * Purpose: Provides the API connection capabilities to the master client
 * Design: Isa ThriftTransporter and FateInterface, therefore it allows for fate operations
 **/
class AccumuloMasterTransporter : public ThriftTransporter, public FateInterface {
 protected:

  // thrift master client interface.
  org::apache::accumulo::core::master::thrift::MasterClientServiceClient *masterClient;

  /**
   * creates a new transporter
   * @return new transporter
   **/
  virtual boost::shared_ptr<apache::thrift::transport::TTransport> recreateTransport() {
    return createTransporter();
  }

  /**
   * Provides a pointer to the master client. Does not provide ownership
   * @return master client interface.
   **/
  org::apache::accumulo::core::master::thrift::MasterClientServiceClient *getMasterClient() {
    return masterClient;
  }

  /**
   * Creates a new master client for this instance.
   *
   **/
  void createMasterClient() {
    boost::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr(new apache::thrift::protocol::TCompactProtocol(underlyingTransport));
    if (NULL != masterClient) {

      delete masterClient;
      masterClient = NULL;
    }
    masterClient = new org::apache::accumulo::core::master::thrift::MasterClientServiceClient(protocolPtr);
  }

  void recreateMasterClient() {
    underlyingTransport->close();
    underlyingTransport.reset();
    if (NULL != masterClient) {

      delete masterClient;
      masterClient = NULL;
    }
    underlyingTransport = createTransporter();
    createMasterClient();
    //createClientService();
  }

 public:

  virtual void registerService(std::string instance, std::string clusterManagers) {
    createMasterClient();
    createClientService();

  }

  explicit AccumuloMasterTransporter(std::shared_ptr<ServerConnection> conn)
      : interconnect::ThriftTransporter(conn),
        interconnect::ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, cclient::data::Range*, cclient::data::Mutation*>(conn) {
    masterClient = NULL;
    createMasterClient();
    createClientService();
  }

  bool createTable(cclient::data::security::AuthInfo *auth, std::string table) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    tableArgs.push_back("MILLIS");
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, org::apache::accumulo::core::master::thrift::FateOperation::TABLE_CREATE, tableArgs, options, true);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::EXISTS:
        default:
          return false;
      }
    }

    return true;

  }

  bool importDirectory(cclient::data::security::AuthInfo *auth, std::string table, std::string dir, std::string failure_dir, bool setTime) {

    std::vector<std::string> tableArgs;

    tableArgs.push_back(table);
    tableArgs.push_back(dir);
    tableArgs.push_back(failure_dir);
    if (__builtin_expect(setTime, false))
      tableArgs.push_back("false");
    else
      tableArgs.push_back("true");

    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, org::apache::accumulo::core::master::thrift::FateOperation::TABLE_BULK_IMPORT, tableArgs, options, false);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::EXISTS:
        default:
          return false;
      }
    } catch (apache::thrift::TApplicationException &e) {
      return false;
    }

    return true;

  }

  bool compactFallBack(cclient::data::security::AuthInfo *auth, std::string table, std::string startrow, std::string endrow, bool wait) {
    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    tableArgs.push_back(startrow);
    tableArgs.push_back(endrow);
    boost::shared_ptr<apache::thrift::transport::TMemoryBuffer> strBuffer(new apache::thrift::transport::TMemoryBuffer());
    boost::shared_ptr<apache::thrift::protocol::TBinaryProtocol> binaryProtcol(new apache::thrift::protocol::TBinaryProtocol(strBuffer));
    org::apache::accumulo::core::tabletserver::thrift::IteratorConfig config;
    config.write(binaryProtcol.get());

    std::string buff = strBuffer->getBufferAsString();
    tableArgs.push_back(buff);

    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, org::apache::accumulo::core::master::thrift::FateOperation::TABLE_COMPACT, tableArgs, options, wait);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      return false;
    } catch (apache::thrift::TApplicationException &e) {
      return false;
    }

    return true;

  }

  bool compact(cclient::data::security::AuthInfo *auth, std::string table, std::string startrow, std::string endrow, bool wait) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    tableArgs.push_back(startrow);
    tableArgs.push_back(endrow);
    boost::shared_ptr<apache::thrift::transport::TMemoryBuffer> strBuffer(new apache::thrift::transport::TMemoryBuffer());
    boost::shared_ptr<apache::thrift::protocol::TBinaryProtocol> binaryProtcol(new apache::thrift::protocol::TBinaryProtocol(strBuffer));
    org::apache::accumulo::core::tabletserver::thrift::IteratorConfig config;
    org::apache::accumulo::core::tabletserver::thrift::TIteratorSetting setting;
    setting.name = "vers";
    setting.priority = 10;
    setting.iteratorClass = "org.apache.accumulo.core.iterators.user.VersioningIterator";
    setting.properties.insert(std::make_pair<std::string, std::string>("maxVersions", "1"));

    //config.iterators.push_back(setting);

    config.write(binaryProtcol.get());

    std::string buff = strBuffer->getBufferAsString();
    tableArgs.push_back(buff);

    std::string clazz = "org.apache.accumulo.tserver.compaction.EverythingCompactionStrategy";
    cclient::data::streams::BigEndianByteStream bout(clazz.size() + 10);

    bout.writeInt(0xcc5e6024);
    bout.writeByte(1);

    bout.writeShort(clazz.size());
    bout.writeBytes(clazz.data(), clazz.size());
    bout.writeInt(0);

    tableArgs.push_back(std::string(bout.getByteArray(), bout.getPos()));

    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, org::apache::accumulo::core::master::thrift::FateOperation::TABLE_COMPACT, tableArgs, options, wait);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      return false;
    } catch (apache::thrift::TApplicationException &e) {
      return compactFallBack(auth, table, startrow, endrow, wait);
    }

    return true;
  }

  bool flush(cclient::data::security::AuthInfo *auth, std::string table, std::string startrow, std::string endrow, bool wait) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    int64_t flushId = 0;
    try {
      flushId = masterClient->initiateFlush(transId, creds, table);

    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      recreateMasterClient();
      return false;
    } catch (apache::thrift::TApplicationException &e) {
      recreateMasterClient();
      return false;
    }

    uint64_t maxLoops = 2147483647;
    if (!wait) {
      maxLoops = 1;
    }

    while (true) {
      try {
        transId.parentId = transId.traceId;
        transId.traceId++;
        recreateMasterClient();
        masterClient->waitForFlush(transId, creds, table, startrow, endrow, flushId, maxLoops);
        break;
      } catch (apache::thrift::transport::TTransportException &e) {
        recreateMasterClient();
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
      } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
        recreateMasterClient();
        return false;
      } catch (apache::thrift::TApplicationException &e) {
        recreateMasterClient();
        return false;
      }
    }

    return true;
  }

  bool removeTable(cclient::data::security::AuthInfo *auth, std::string table) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, org::apache::accumulo::core::master::thrift::FateOperation::TABLE_DELETE, tableArgs, options);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::EXISTS:
        default:
          return false;
      }
    }

    return true;

  }

  void removeTableProperty(cclient::data::security::AuthInfo *auth, std::string table, std::string property) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClient->removeTableProperty(transId, creds, table, property);
  }

  void setTableProperty(cclient::data::security::AuthInfo *auth, std::string table, std::string property, std::string value) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClient->setTableProperty(transId, creds, table, property, value);
  }

  /**namespace operations**/

  bool createNamespace(cclient::data::security::AuthInfo *auth, std::string name) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(name);
    //tableArgs.push_back ("MILLIS");
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, org::apache::accumulo::core::master::thrift::FateOperation::NAMESPACE_CREATE, tableArgs, options, true);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::NAMESPACE_NOTFOUND:
        default:
          return false;
      }
    }

    return true;

  }

  bool deletenamespace(cclient::data::security::AuthInfo *auth, std::string name) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(name);
    //tableArgs.push_back ("MILLIS");
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, org::apache::accumulo::core::master::thrift::FateOperation::NAMESPACE_DELETE, tableArgs, options, true);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::NAMESPACE_NOTFOUND:
        default:
          return false;
      }
    }

    return true;

  }

  bool renamenamespace(cclient::data::security::AuthInfo *auth, std::string oldName, std::string newName) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(oldName);
    tableArgs.push_back(newName);
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, org::apache::accumulo::core::master::thrift::FateOperation::NAMESPACE_RENAME, tableArgs, options, true);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::NAMESPACE_NOTFOUND:
        default:
          return false;
      }
    }

    return true;

  }

  std::map<std::string, std::string> getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, std::string nameSpaceName) {
    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);

    tinfo.parentId = 0;
    tinfo.traceId = rand();
    std::map<std::string, std::string> ret;
    client->getNamespaceConfiguration(ret, tinfo, creds, nameSpaceName);

    return ret;
  }

  void removeNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, std::string property) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClient->removeNamespaceProperty(transId, creds, nameSpaceName, property);
  }

  void setNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, std::string property, std::string value) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClient->setNamespaceProperty(transId, creds, nameSpaceName, property, value);
  }

  virtual ~AccumuloMasterTransporter() {
    if (NULL != masterClient)
      delete masterClient;
  }
};

} /* namespace data */

#endif /* SRC_INTERCONNECT_TRANSPORT_ACCUMULOMASTERTRANSPOTER_H_ */

