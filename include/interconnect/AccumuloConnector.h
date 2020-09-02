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

#ifndef SRC_INTERCONNECT_ACCUMULOBASECONNECTOR_H_
#define SRC_INTERCONNECT_ACCUMULOBASECONNECTOR_H_

#include "../data/constructs/IterInfo.h"
#include "../data/constructs/configuration/Configuration.h"
#include "../data/constructs/inputvalidation.h"
#include "../data/constructs/scanstate.h"
#include "../data/constructs/server/RangeDefinition.h"
#include "../data/constructs/server/ServerDefinition.h"
#include "../data/constructs/tablet/TabletType.h"
#include "../data/exceptions/ClientException.h"
#include "../data/exceptions/IllegalArgumentException.h"
#include "../data/extern/thrift/data_types.h"
#include "../interconnect/ClientInterface.h"
#include "TransportPool.h"

namespace interconnect {

/**
 * Accumulo Connector
 */
template <typename Tr>
class AccumuloBaseConnector : virtual public ClientInterface<Tr> {
 public:
  /**
   * base constructor
   **/
  AccumuloBaseConnector()
      : ClientInterface<Tr>(),
        myTransportPool(NULL),
        rangeDef(NULL),
        tServer(NULL),
        serverDef(NULL) {}
  /**
   * Constructor accepting host and port
   * @param host host name
   * @param port port for Accumulo server
   **/
  AccumuloBaseConnector(const std::string host, const int port)
      : ClientInterface<Tr>(host, port),
        myTransportPool(NULL),
        rangeDef(NULL),
        tServer(NULL),
        serverDef(NULL) {}
  /**
   * Destructor with no ownership guidelines
   **/
  virtual ~AccumuloBaseConnector() {}

  cclient::data::security::AuthInfo *getCredentials() { return &credentials; }

  std::shared_ptr<cclient::data::tserver::RangeDefinition>
  getRangesDefinition() {
    return rangeDef;
  }

 protected:
  TransportPool<Tr> *myTransportPool;
  cclient::data::security::AuthInfo credentials;
  std::shared_ptr<cclient::data::tserver::RangeDefinition> rangeDef;
  std::shared_ptr<ServerConnection> tServer;
  std::shared_ptr<cclient::data::tserver::ServerDefinition> serverDef;
};

} /* namespace interconnect */

#endif /* SRC_INTERCONNECT_ACCUMULOBASECONNECTOR_H_ */
