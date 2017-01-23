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

#include "../../concurrency/ThreadPool.h"
#include "../../concurrency/Executor.h"
#include "../../concurrency/ReturnType.h"

namespace services
{
class Server
{
protected:
    ThreadPool *executor;
public:
    Server() {
        executor = new ThreadPool();
    }

    void open()
    {
        //boost::shared_ptr<TTransport> socket(new TSocket("localhost", 9090));
        //boost::shared_ptr<TTransport> transport(new TBufferedTransport(socket));
        //boost::shared_ptr<TProtocol> protocol(new TBinaryProtocol(transport));
    }
};
}

#endif /* INCLUDE_DATA_SERVICES_SERVER_H_ */
