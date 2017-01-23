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
#ifndef INCLUDE_DATA_SERVICES_SERVER_H_
#define INCLUDE_DATA_SERVICES_SERVER_H_


#include "../../include/concurrency/ThreadPool.h"
#include "../../include/concurrency/Executor.h"
#include "../../include/concurrency/ReturnType.h"

#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>

#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransportException.h>
#include <transport/TBufferTransports.h>


#include <server/TThreadedServer.h>
#include <transport/TServerSocket.h>


#include <chrono>
#include <thread>
#include <pthread.h>
#include <sys/time.h>

#include "../../include/data/services/accumulo/TestTabletServer.h"

using namespace std;
using namespace ::apache::thrift;
using namespace ::apache::thrift::protocol;
using namespace ::apache::thrift::transport;
using namespace ::apache::thrift::server;
using namespace services::accumulo;

class MockServer
{
protected:
    ThreadPool *executor;
    uint16_t port;
public:
    MockServer(int port) : port(port){
        executor = new ThreadPool();
    }

    void open()
    {
      shared_ptr<TProcessor> processor(new TestTabletServer());
      shared_ptr<TServerTransport> serverTransport(new TServerSocket(port));
      shared_ptr<TTransportFactory> transportFactory(new TBufferedTransportFactory());
      shared_ptr<TProtocolFactory> protocolFactory(new TBinaryProtocolFactory());

      // start the threaded server.
      TThreadedServer server(processor, serverTransport, transportFactory, protocolFactory);
      server.serve();
    }
    
    
};


#endif /* INCLUDE_DATA_SERVICES_SERVER_H_ */
