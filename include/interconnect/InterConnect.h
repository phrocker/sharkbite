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

#ifndef INTERCONNECT_H_
#define INTERCONNECT_H_ 1

#include <string>
#include <map>
#include <set>
#include <algorithm>    // std::random_shuffle
#include <vector>       // std::vector
#include <ctime>        // std::time
#include <cstdlib>      // std::rand, std::srand

#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>

#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransportException.h>
#include <transport/TBufferTransports.h>

#include <concurrency/ThreadManager.h>

#include <sys/time.h>

#include "../data/exceptions/ClientException.h"
#include "../data/exceptions/IllegalArgumentException.h"
#include "../data/constructs/inputvalidation.h"
#include "transport/BaseTransport.h"
#include "transport/ServerConnection.h"
#include "transport/CachedTransport.h"

namespace interconnect {

#define TSERVER_PORT_OPT "tserver.port.client"
#define TSERVER_DEFAULT_PORT 9997

#define GENERAL_RPC_TIMEOUT_OPT "general.rpc.timeout"
#define GENERAL_RPC_TIMEOUT 120*1000

enum INTERCONNECT_TYPES {
  TSERV_CLIENT,
  MASTER_CLIENT,
  GC_CLIENT
};

#define SERVICE_SEPARATOR ';'
#define SEPARATOR_CHAR '='

template<typename T>
class EnumParser {
 protected:
  std::map<std::string, T> enumMap;
 public:
  EnumParser() {
  }

  T ParseSomeEnum(const std::string &value) {
    auto iValue = enumMap.find(value);
    if (iValue == enumMap.end())
      throw cclient::exceptions::IllegalArgumentException("");
    return iValue->second;
  }
};

class TYPE_PARSER : public EnumParser<INTERCONNECT_TYPES> {
 public:
  TYPE_PARSER() {
    enumMap["TSERV_CLIENT"] = INTERCONNECT_TYPES::TSERV_CLIENT;
    enumMap["tserv_client"] = INTERCONNECT_TYPES::TSERV_CLIENT;
    enumMap["tserver"] = INTERCONNECT_TYPES::TSERV_CLIENT;

    enumMap["MASTER_CLIENT"] = INTERCONNECT_TYPES::MASTER_CLIENT;
    enumMap["master_client"] = INTERCONNECT_TYPES::MASTER_CLIENT;
    enumMap["master"] = INTERCONNECT_TYPES::MASTER_CLIENT;

    enumMap["GC_CLIENT"] = INTERCONNECT_TYPES::GC_CLIENT;
    enumMap["gc_client"] = INTERCONNECT_TYPES::GC_CLIENT;
    enumMap["gc"] = INTERCONNECT_TYPES::GC_CLIENT;
  }
};

/**
 * Connector service instance
 * Purpose: Acts as a reference within the map of known connection types
 *
 */
class ConnectorService {
 public:
  ConnectorService(std::string service, std::string server, uint16_t port) {
    serviceMap.insert(std::pair<INTERCONNECT_TYPES, std::string>(parser.ParseSomeEnum(service), server));

  }

  std::string getAddressString(INTERCONNECT_TYPES type) {
    return serviceMap[type];
  }

 protected:
  std::map<INTERCONNECT_TYPES, std::string> serviceMap;
  TYPE_PARSER parser;
};

#define ERROR_THRESHOLD 20L
#define STUCK_THRESHOLD 2*60*1000

}
#endif
