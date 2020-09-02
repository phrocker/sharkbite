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
#include "interconnect/InterConnect.h"

#include <concurrency/ThreadManager.h>
#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>
#include <sys/time.h>
#include <transport/TBufferTransports.h>
#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransport.h>
#include <transport/TTransportException.h>

#include <algorithm>  // std::random_shuffle
#include <cstdlib>    // std::rand, std::srand
#include <ctime>      // std::time
#include <map>
#include <set>
#include <string>
#include <vector>  // std::vector

#include "interconnect/../data/constructs/inputvalidation.h"
#include "interconnect/../data/exceptions/ClientException.h"
#include "interconnect/../data/exceptions/IllegalArgumentException.h"
#include "interconnect/transport/BaseTransport.h"
#include "interconnect/transport/CachedTransport.h"
#include "interconnect/transport/ServerConnection.h"
