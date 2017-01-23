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

#ifndef SRC_INTERCONNECT_FATEINTERFACE_H_
#define SRC_INTERCONNECT_FATEINTERFACE_H_


#include <concurrency/ThreadManager.h>

#include <chrono>
#include <thread>
#include <sys/time.h>

#include <map>
#include <set>
#include <string>
#include <algorithm>    // std::random_shuffle
#include <vector>       // std::vector
#include <ctime>        // std::time
#include <cstdlib>      // std::rand, std::srand





#include "../../data/constructs/inputvalidation.h"
#include "../../data/constructs/IterInfo.h"
#include "../../data/constructs/configuration/Configuration.h"
#include "../../data/extern/thrift/data_types.h"
#include "../../data/extern/thrift/tabletserver_types.h"
#include "../../data/constructs/scanstate.h"
#include "../../data/exceptions/ClientException.h"
#include "../../data/exceptions/IllegalArgumentException.h"
#include "../../data/constructs/tablet/TabletType.h"
#include "../scanrequest/ScanIdentifier.h"

#include "Transport.h"
#include <boost/concept_check.hpp>
#include <boost/shared_ptr.hpp>

#include "../../data/extern/thrift/ClientService.h"
#include "../../data/extern/thrift/master_types.h"
#include "../../data/extern/thrift/MasterClientService.h"
#include "../../data/extern/thrift/ThriftWrapper.h"
#include "../../data/constructs/security/AuthInfo.h"
#include "../Scan.h"




//#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>

#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransportException.h>
#include <transport/TBufferTransports.h>

#include "MasterInterface.h"
namespace interconnect
{

/**
 * Fate implementation class
 **/
class FateInterface : public MasterInterface
{
protected:
  /**
   * Executes fate operations. 
   * @param auth authorization info
   * @param type fate operation
   * @param tableArgs namespace or table arguments
   * @param options options for this fate operation
   * @param wait determines if we will wait on the fate operation
   * @return return value of the fate operation
   **/ 
  std::string
	doFateOperations (
	        cclient::data::security::AuthInfo *auth,
	        org::apache::accumulo::core::master::thrift::FateOperation::type type,
	        std::vector<std::string> tableArgs, std::map<std::string,std::string> options, bool wait = false);
};

}

#endif
