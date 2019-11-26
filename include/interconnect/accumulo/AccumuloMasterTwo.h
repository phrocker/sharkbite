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

#include "AccumuloMasterFacade.h"
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

class AccumuloMasterFacadeV2 : public AccumuloMasterFacade {

 private:

  std::shared_ptr<logging::Logger> logger;

  std::shared_ptr<org::apache::accumulov2::core::master::thrift::MasterClientServiceClient> masterClientV2;

  bool v2_createTable(cclient::data::security::AuthInfo *auth, const std::string &table) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    tableArgs.push_back("MILLIS");
    tableArgs.push_back("ONLINE");
    tableArgs.push_back("0");
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::TABLE_CREATE, tableArgs, options, true);
    } catch (org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulov2::core::clientImpl::thrift::TableOperationExceptionType::EXISTS:
        default:
          return false;
      }
    }

    return true;

  }

  bool v2_importDirectory(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &dir, std::string failure_dir, bool setTime) {

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
    } catch (org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulov2::core::clientImpl::thrift::TableOperationExceptionType::EXISTS:
        default:
          return false;
      }
    } catch (apache::thrift::TApplicationException &e) {
      return false;
    }

    return true;

  }

  bool v2_compactFallBack(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    tableArgs.push_back(startrow);
    tableArgs.push_back(endrow);
    std::shared_ptr<apache::thrift::transport::TMemoryBuffer> strBuffer(new apache::thrift::transport::TMemoryBuffer());
    std::shared_ptr<apache::thrift::protocol::TBinaryProtocol> binaryProtcol(new apache::thrift::protocol::TBinaryProtocol(strBuffer));
    org::apache::accumulov2::core::tabletserver::thrift::IteratorConfig config;
    config.write(binaryProtcol.get());

    std::string buff = strBuffer->getBufferAsString();
    tableArgs.push_back(buff);

    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::TABLE_COMPACT, tableArgs, options, wait);
    } catch (org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
      return false;
    } catch (apache::thrift::TApplicationException &e) {
      return false;
    }

    return true;

  }

  bool v2_compact(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    tableArgs.push_back(startrow);
    tableArgs.push_back(endrow);
    std::shared_ptr<apache::thrift::transport::TMemoryBuffer> strBuffer(new apache::thrift::transport::TMemoryBuffer());
    std::shared_ptr<apache::thrift::protocol::TBinaryProtocol> binaryProtcol(new apache::thrift::protocol::TBinaryProtocol(strBuffer));
    org::apache::accumulov2::core::tabletserver::thrift::IteratorConfig config;
    org::apache::accumulov2::core::tabletserver::thrift::TIteratorSetting setting;
    setting.name = "vers";
    setting.priority = 10;
    setting.iteratorClass = "org.apache.accumulo.core.iterators.user.VersioningIterator";

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
    } catch (org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
      return false;
    } catch (const apache::thrift::TApplicationException &e) {
      return compactFallBack(auth, table, startrow, endrow, wait);
    }

    return true;
  }

  bool v2_flush(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    org::apache::accumulov2::core::trace::thrift::TInfo transId;
    org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    int64_t flushId = 0;
    try {
      flushId = masterClientV2->initiateFlush(transId, creds, table);

    } catch (const org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
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
        masterClientV2->waitForFlush(transId, creds, table, startrow, endrow, flushId, maxLoops);
        break;
      } catch (const apache::thrift::transport::TTransportException &e) {
        recreateMasterTransport();
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
      } catch (const org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
        recreateMasterTransport();
        return false;
      } catch (const apache::thrift::TApplicationException &e) {
        recreateMasterTransport();
        return false;
      }
    }

    return true;
  }

  bool v2_removeTable(cclient::data::security::AuthInfo *auth, const std::string &table) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(table);
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::TABLE_DELETE, tableArgs, options);
    } catch (org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulov2::core::clientImpl::thrift::TableOperationExceptionType::EXISTS:
        default:
          return false;
      }
    }

    return true;

  }

  void v2_removeTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property) {
    org::apache::accumulov2::core::trace::thrift::TInfo transId;
    org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClientV2->removeTableProperty(transId, creds, table, property);
  }

  void v2_setTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property, const std::string &value) {
    org::apache::accumulov2::core::trace::thrift::TInfo transId;
    org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClientV2->setTableProperty(transId, creds, table, property, value);
  }

  /**namespace operations**/

  bool v2_createNamespace(cclient::data::security::AuthInfo *auth, std::string name) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(name);
    //tableArgs.push_back ("MILLIS");
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::NAMESPACE_CREATE, tableArgs, options, true);
    } catch (org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulov2::core::clientImpl::thrift::TableOperationExceptionType::NAMESPACE_NOTFOUND:
        default:
          return false;
      }
    }

    return true;

  }

  bool v2_deletenamespace(cclient::data::security::AuthInfo *auth, std::string name) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(name);
    //tableArgs.push_back ("MILLIS");
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::NAMESPACE_DELETE, tableArgs, options, true);
    } catch (org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulov2::core::clientImpl::thrift::TableOperationExceptionType::NAMESPACE_NOTFOUND:
        default:
          return false;
      }
    }

    return true;

  }

  bool v2_renamenamespace(cclient::data::security::AuthInfo *auth, std::string oldName, std::string newName) {

    std::vector<std::string> tableArgs;
    tableArgs.push_back(oldName);
    tableArgs.push_back(newName);
    std::map<std::string, std::string> options;
    try {
      std::string returnValue = doFateOperations(auth, AccumuloFateOperation::NAMESPACE_RENAME, tableArgs, options, true);
    } catch (org::apache::accumulov2::core::clientImpl::thrift::ThriftTableOperationException &e) {
      switch (e.type) {
        case org::apache::accumulov2::core::clientImpl::thrift::TableOperationExceptionType::NAMESPACE_NOTFOUND:
        default:
          return false;
      }
    }

    return true;

  }

  void v2_removeNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property) {
    org::apache::accumulov2::core::trace::thrift::TInfo transId;
    org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClientV2->removeNamespaceProperty(transId, creds, nameSpaceName, property);
  }

  void v2_setNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property, const std::string &value) {
    org::apache::accumulov2::core::trace::thrift::TInfo transId;
    org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);
    transId.parentId = 0;
    transId.traceId = rand();

    masterClientV2->setNamespaceProperty(transId, creds, nameSpaceName, property, value);
  }

  void v2_createMasterClient(std::shared_ptr<apache::thrift::transport::TTransport> underlyingTransport) {
    auto protocolPtr = std::make_shared<apache::thrift::protocol::TCompactProtocol>(underlyingTransport);
    masterClientV2 = std::make_shared<org::apache::accumulov2::core::master::thrift::MasterClientServiceClient>(protocolPtr);
  }

 public:

  AccumuloMasterFacadeV2(const std::string &host, std::function<void()> fx, std::function<std::shared_ptr<apache::thrift::transport::TTransport>()> tfx)
      : AccumuloMasterFacade(host, ACCUMULO_TWO, fx, tfx),
        logger(logging::LoggerFactory<AccumuloMasterFacadeV2>::getLogger()) {
  }

  void createMasterClient(std::shared_ptr<apache::thrift::transport::TTransport> underlyingTransport) {
    v2_createMasterClient(underlyingTransport);
  }

  std::string doFateOperations(cclient::data::security::AuthInfo *auth, AccumuloFateOperation mytype, const std::vector<std::string> &tableArgs, const std::map<std::string, std::string> &options,
                               bool wait = false) {
    std::string ret;
    ret = v2_doFateOperations(auth, mytype, tableArgs, options, wait);
    return ret;
  }

  std::string v2_doFateOperations(cclient::data::security::AuthInfo *auth, AccumuloFateOperation mytype, const std::vector<std::string> &tableArgs, const std::map<std::string, std::string> &options,
                                  bool wait = false) {
    auto myMasterClient = masterClientV2;

    org::apache::accumulov2::core::master::thrift::FateOperation::type type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_CREATE;
    switch (mytype) {
      case TABLE_CREATE:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_CREATE;
        break;
      case TABLE_CLONE:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_CLONE;
        break;
      case TABLE_DELETE:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_DELETE;
        break;
      case TABLE_RENAME:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_RENAME;
        break;
      case TABLE_ONLINE:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_ONLINE;
        break;
      case TABLE_OFFLINE:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_OFFLINE;
        break;
      case TABLE_MERGE:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_MERGE;
        break;
      case TABLE_DELETE_RANGE:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_DELETE_RANGE;
        break;
      case TABLE_BULK_IMPORT:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_BULK_IMPORT;
        break;
      case TABLE_COMPACT:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_COMPACT;
        break;
      case TABLE_IMPORT:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_IMPORT;
        break;
      case TABLE_EXPORT:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_EXPORT;
        break;
      case TABLE_CANCEL_COMPACT:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::TABLE_CANCEL_COMPACT;
        break;
      case NAMESPACE_CREATE:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::NAMESPACE_CREATE;
        break;
      case NAMESPACE_DELETE:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::NAMESPACE_DELETE;
        break;
      case NAMESPACE_RENAME:
        type = org::apache::accumulov2::core::master::thrift::FateOperation::type::NAMESPACE_RENAME;
        break;
      default:
        return "";
    };
    org::apache::accumulov2::core::trace::thrift::TInfo transId;
    org::apache::accumulov2::core::securityImpl::thrift::TCredentials creds = ThriftV2Wrapper::convert(auth);
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
        logging::LOG_ERROR(logger) << "Error during fate " << e.what();
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

          auto protocolPtr = std::make_shared<apache::thrift::protocol::TCompactProtocol>(myTransport);
          org::apache::accumulov2::core::master::thrift::MasterClientServiceClient waitClient(protocolPtr);
          waitClient.waitForFateOperation(returnValue, transId, creds, fateTransId);

          break;

        } catch (apache::thrift::transport::TTransportException &e) {
          logging::LOG_ERROR(logger) << "Error during fate " << e.what();
          std::this_thread::sleep_for(std::chrono::milliseconds(100));
          myTransport->close();
        } catch (std::runtime_error &e) {
          logging::LOG_ERROR(logger) << "Error during fate " << e.what();
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
    return v2_createTable(auth, table);

  }

  bool importDirectory(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &dir, std::string failure_dir, bool setTime) {
    return v2_importDirectory(auth, table, dir, failure_dir, setTime);
  }

  bool compactFallBack(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    return v2_compactFallBack(auth, table, startrow, endrow, wait);
  }

  bool compact(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    return v2_compact(auth, table, startrow, endrow, wait);
  }

  bool flush(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    return v2_flush(auth, table, startrow, endrow, wait);
  }

  bool removeTable(cclient::data::security::AuthInfo *auth, const std::string &table) {
    return v2_removeTable(auth, table);

  }

  void removeTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property) {
    v2_removeTableProperty(auth, table, property);
  }

  void setTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property, const std::string &value) {
    v2_setTableProperty(auth, table, property, value);\
  }

  /**namespace operations**/

  bool createNamespace(cclient::data::security::AuthInfo *auth, std::string name) {
    return v2_createNamespace(auth, name);
  }

  bool deletenamespace(cclient::data::security::AuthInfo *auth, std::string name) {
    return v2_deletenamespace(auth, name);
  }

  bool renamenamespace(cclient::data::security::AuthInfo *auth, std::string oldName, std::string newName) {
    return v2_renamenamespace(auth, oldName, newName);
  }

  void removeNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property) {
    v2_removeNamespaceProperty(auth, nameSpaceName, property);
  }

  void setNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property, const std::string &value) {
    v2_setNamespaceProperty(auth, nameSpaceName, property, value);
  }

};

}
