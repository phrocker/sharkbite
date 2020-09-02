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

#ifndef SRC_INTERCONNECT_ACCUMULO_ACCUMULOSTREAMS_H_
#define SRC_INTERCONNECT_ACCUMULO_ACCUMULOSTREAMS_H_

#include "../../scanner/impl/Scanner.h"
#include "../../writer/impl/SinkImpl.h"

namespace interconnect {
class AccumuloStreams : public scanners::Scanner, public writer::Writer {
 public:
  AccumuloStreams(
      std::shared_ptr<cclient::data::Instance> instance,
      TableOperations<cclient::data::KeyValue,
                      scanners::ResultBlock<cclient::data::KeyValue>> *tops,
      cclient::data::security::Authorizations *auths, uint16_t threads);
  virtual ~AccumuloStreams();
};

}  // namespace interconnect

#endif /* SRC_INTERCONNECT_ACCUMULO_ACCUMULOSTREAMS_H_ */
