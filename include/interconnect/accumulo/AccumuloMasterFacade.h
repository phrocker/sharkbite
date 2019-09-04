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
#include "data/extern/thrift/TabletClientService.h"
#include "../scanrequest/ScanRequest.h"
#include "../scanrequest/ScanIdentifier.h"
#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/master_types.h"
#include "data/extern/thrift/MasterClientService.h"
#include "data/extern/thrift/ThriftWrapper.h"

#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"

namespace interconnect {

class AccumuloMasterFacade {

 private:
  std::shared_ptr<org::apache::accumulo::core::master::thrift::MasterClientServiceClient> masterClient;

  uint8_t accumuloVersion;

  std::function<std::shared_ptr<apache::thrift::transport::TTransport>()> createTransport;
  std::function<void()> createMasterTransport;

  void v1_createMasterClient(boost::shared_ptr<apache::thrift::transport::TTransport> underlyingTransport) {
    auto protocolPtr = std::make_shared<apache::thrift::protocol::TCompactProtocol>(underlyingTransport);
    //boost::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr(new apache::thrift::protocol::TCompactProtocol(underlyingTransport));
    //if (NULL != masterClient) {

    //      delete masterClient;
    //    masterClient = NULL;
    //    }
    masterClient = std::make_shared<org::apache::accumulo::core::master::thrift::MasterClientServiceClient>(boost::tools::from_shared_ptr<apache::thrift::protocol::TCompactProtocol>(protocolPtr));
  }

  bool v1_createTable(cclient::data::security::AuthInfo *auth, const std::string &table) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    tableArgs.push_back("MILLIS");
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::TABLE_CREATE, tableArgs, options, true);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::EXISTS:
        default:
          return false;
      }
    }

    return true;

  }

  bool v1_importDirectory(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &dir, std::string failure_dir, bool setTime) {

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
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::TABLE_BULK_IMPORT, tableArgs, options, false);
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

  bool v1_compactFallBack(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
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
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::TABLE_COMPACT, tableArgs, options, wait);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      return false;
    } catch (apache::thrift::TApplicationException &e) {
      return false;
    }

    return true;

  }

  bool v1_compact(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {

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
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::TABLE_COMPACT, tableArgs, options, wait);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      return false;
    } catch (const apache::thrift::TApplicationException &e) {
      return compactFallBack(auth, table, startrow, endrow, wait);
    }

    return true;
  }

  bool v1_flush(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    int64_t flushId = 0;
    try {
      flushId = masterClient->initiateFlush(transId, creds, table);

    } catch (const org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      recreateMasterTransport();
      return false;
    } catch (const apache::thrift::TApplicationException &e) {
      recreateMasterTransport();
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
        recreateMasterTransport();
        masterClient->waitForFlush(transId, creds, table, startrow, endrow, flushId, maxLoops);
        break;
      } catch (const apache::thrift::transport::TTransportException &e) {
        recreateMasterTransport();
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
      } catch (const org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
        recreateMasterTransport();
        return false;
      } catch (const apache::thrift::TApplicationException &e) {
        recreateMasterTransport();
        return false;
      }
    }

    return true;
  }

  bool v1_removeTable(cclient::data::security::AuthInfo *auth, const std::string &table) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::TABLE_DELETE, tableArgs, options);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::EXISTS:
        default:
          return false;
      }
    }

    return true;

  }

  void v1_removeTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClient->removeTableProperty(transId, creds, table, property);
  }

  void v1_setTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property, const std::string &value) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClient->setTableProperty(transId, creds, table, property, value);
  }

  /**namespace operations**/

  bool v1_createNamespace(cclient::data::security::AuthInfo *auth, std::string name) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(name);
    //tableArgs.push_back ("MILLIS");
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::NAMESPACE_CREATE, tableArgs, options, true);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::NAMESPACE_NOTFOUND:
        default:
          return false;
      }
    }

    return true;

  }

  bool v1_deletenamespace(cclient::data::security::AuthInfo *auth, std::string name) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(name);
    //tableArgs.push_back ("MILLIS");
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::NAMESPACE_DELETE, tableArgs, options, true);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::NAMESPACE_NOTFOUND:
        default:
          return false;
      }
    }

    return true;

  }

  bool v1_renamenamespace(cclient::data::security::AuthInfo *auth, std::string oldName, std::string newName) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(oldName);
    tableArgs.push_back(newName);
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::NAMESPACE_RENAME, tableArgs, options, true);
    } catch (org::apache::accumulo::core::client::impl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulo::core::client::impl::thrift::TableOperationExceptionType::NAMESPACE_NOTFOUND:
        default:
          return false;
      }
    }

    return true;

  }

  void v1_removeNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClient->removeNamespaceProperty(transId, creds, nameSpaceName, property);
  }

  void v1_setNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property, const std::string &value) {
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClient->setNamespaceProperty(transId, creds, nameSpaceName, property, value);
  }

 public:

  AccumuloMasterFacade(std::function<void()> fx, std::function<std::shared_ptr<apache::thrift::transport::TTransport>()> tfx)
      : createMasterTransport(fx),
        createTransport(tfx),
        accumuloVersion(ACCUMULO_UNKNOWN) {
    accumuloVersion = ACCUMULO_ONE;
  }

  void createMasterClient(boost::shared_ptr<apache::thrift::transport::TTransport> underlyingTransport) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        v1_createMasterClient(underlyingTransport);
        break;
    }
  }

  void recreateMasterTransport() {
    createMasterTransport();
  }

  std::string doFateOperations(cclient::data::security::AuthInfo *auth, AccumuloFateOperation mytype, const std::vector<std::string> &tableArgs, const std::map<std::string, std::string> &options,
                               bool wait = false) {
    std::string ret;
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        ret = v1_doFateOperations(auth, mytype, tableArgs, options, wait);
        break;
    }
    return ret;
  }

  std::string v1_doFateOperations(cclient::data::security::AuthInfo *auth, AccumuloFateOperation mytype, const std::vector<std::string> &tableArgs, const std::map<std::string, std::string> &options,
                                  bool wait = false) {
    auto myMasterClient = masterClient;

    org::apache::accumulo::core::master::thrift::FateOperation::type type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_CREATE;
    switch (mytype) {
      case TABLE_CREATE:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_CREATE;
        break;
      case TABLE_CLONE:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_CLONE;
        break;
      case TABLE_DELETE:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_DELETE;
        break;
      case TABLE_RENAME:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_RENAME;
        break;
      case TABLE_ONLINE:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_ONLINE;
        break;
      case TABLE_OFFLINE:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_OFFLINE;
        break;
      case TABLE_MERGE:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_MERGE;
        break;
      case TABLE_DELETE_RANGE:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_DELETE_RANGE;
        break;
      case TABLE_BULK_IMPORT:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_BULK_IMPORT;
        break;
      case TABLE_COMPACT:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_COMPACT;
        break;
      case TABLE_IMPORT:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_IMPORT;
        break;
      case TABLE_EXPORT:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_EXPORT;
        break;
      case TABLE_CANCEL_COMPACT:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::TABLE_CANCEL_COMPACT;
        break;
      case NAMESPACE_CREATE:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::NAMESPACE_CREATE;
        break;
      case NAMESPACE_DELETE:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::NAMESPACE_DELETE;
        break;
      case NAMESPACE_RENAME:
        type = org::apache::accumulo::core::master::thrift::FateOperation::type::NAMESPACE_RENAME;
        break;
      default:
        return "";
    };
    org::apache::accumulo::core::trace::thrift::TInfo transId;
    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();
    int64_t fateTransId = myMasterClient->beginFateOperation(transId, creds);

    transId.parentId = transId.traceId;
    transId.traceId = transId.traceId + 1;

    bool succeeded = false;
    while (!succeeded) {

      try {
        myMasterClient->executeFateOperation(transId, creds, fateTransId, type, tableArgs, options, !wait);
        succeeded = true;
      } catch (apache::thrift::transport::TTransportException &e) {
        std::cout << e.what() << std::endl;
        recreateMasterTransport();
        succeeded = false;
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
      }

    }

    std::string returnValue;
    if (wait) {
      while (true) {
        transId.parentId = transId.traceId;
        transId.traceId = transId.traceId + 1;
        auto myTransport = createTransport();
        try {

          auto protocolPtr = std::make_shared<apache::thrift::protocol::TCompactProtocol>(boost::tools::from_shared_ptr<apache::thrift::transport::TTransport>(myTransport));
          org::apache::accumulo::core::master::thrift::MasterClientServiceClient waitClient(boost::tools::from_shared_ptr<apache::thrift::protocol::TCompactProtocol>(protocolPtr));
          waitClient.waitForFateOperation(returnValue, transId, creds, fateTransId);

          break;

        } catch (apache::thrift::transport::TTransportException &e) {
          std::cout << e.what() << std::endl;
          std::this_thread::sleep_for(std::chrono::milliseconds(100));
          myTransport->close();
        } catch (std::runtime_error &e) {
          std::cout << e.what() << std::endl;
          myTransport->close();
          throw e;
        }
        myTransport->close();

      }

      transId.parentId = transId.traceId;
      transId.traceId = transId.traceId + 1;
      myMasterClient->finishFateOperation(transId, creds, fateTransId);
      recreateMasterTransport();
    }
    return returnValue;
  }

  bool createTable(cclient::data::security::AuthInfo *auth, const std::string &table) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_createTable(auth, table);
        break;
    }
    return false;
  }

  bool importDirectory(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &dir, std::string failure_dir, bool setTime) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_importDirectory(auth, table, dir, failure_dir, setTime);
        break;
    }
    return false;
  }

  bool compactFallBack(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_compactFallBack(auth, table, startrow, endrow, wait);
        break;
    }
    return false;
  }

  bool compact(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_compact(auth, table, startrow, endrow, wait);
        break;
    }
    return false;
  }

  bool flush(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_flush(auth, table, startrow, endrow, wait);
        break;
    }
    return false;
  }

  bool removeTable(cclient::data::security::AuthInfo *auth, const std::string &table) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_removeTable(auth, table);
        break;
    }
    return false;
  }

  void removeTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        v1_removeTableProperty(auth, table, property);
        break;
    }
  }

  void setTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property, const std::string &value) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        v1_setTableProperty(auth, table, property, value);
        break;
    }
  }

  /**namespace operations**/

  bool createNamespace(cclient::data::security::AuthInfo *auth, std::string name) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_createNamespace(auth, name);
        break;
    }
    return false;
  }

  bool deletenamespace(cclient::data::security::AuthInfo *auth, std::string name) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_deletenamespace(auth, name);
        break;
    }
    return false;
  }

  bool renamenamespace(cclient::data::security::AuthInfo *auth, std::string oldName, std::string newName) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        return v1_renamenamespace(auth, oldName, newName);
        break;
    }
    return false;
  }

  void removeNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        v1_removeNamespaceProperty(auth, nameSpaceName, property);
        break;
    }
  }

  void setNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property, const std::string &value) {
    switch (accumuloVersion) {
      case ACCUMULO_ONE:
        v1_setNamespaceProperty(auth, nameSpaceName, property, value);
        break;
    }
  }

};

}
