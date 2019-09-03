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

#include "interconnect/transport/FateInterface.h"
#include "data/extern/boost/SharedPointer.h"

namespace interconnect {

std::string FateInterface::doFateOperations(cclient::data::security::AuthInfo *auth, org::apache::accumulo::core::master::thrift::FateOperation::type type, const std::vector<std::string> &tableArgs,
                                            const std::map<std::string, std::string> &options, bool wait) {

  auto myMasterClient = getMasterClient();
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
      recreateMasterClient();
      succeeded = false;
      std::this_thread::sleep_for(std::chrono::milliseconds(100));
    }

  }

  std::string returnValue;
  if (wait) {
    while (true) {
      transId.parentId = transId.traceId;
      transId.traceId = transId.traceId + 1;
      auto myTransport = recreateTransport();
      try {

        auto protocolPtr = std::make_shared < apache::thrift::protocol::TCompactProtocol > (boost::tools::from_shared_ptr<apache::thrift::transport::TTransport>(myTransport));
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
    recreateMasterClient();
  }
  return returnValue;
}
}
