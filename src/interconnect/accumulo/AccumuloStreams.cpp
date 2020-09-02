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
#include "interconnect/accumulo/AccumuloStreams.h"

#include "interconnect/accumulo/../../scanner/impl/Scanner.h"
#include "interconnect/accumulo/../../writer/impl/SinkImpl.h"

namespace interconnect {

AccumuloStreams::AccumuloStreams(
    std::shared_ptr<cclient::data::Instance> instance,
    TableOperations<cclient::data::KeyValue,
                    scanners::ResultBlock<cclient::data::KeyValue>> *tops,
    cclient::data::security::Authorizations *auths, uint16_t threads)
    : scanners::Scanner(instance, tops, auths, threads),
      writer::Writer(instance, tops, auths, threads) {}

AccumuloStreams::~AccumuloStreams() {}

} /* namespace interconnect */
