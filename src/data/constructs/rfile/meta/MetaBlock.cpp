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

#include "data/constructs/rfile/meta/MetaBlock.h"

#include "data/constructs/rfile/meta/../../../streaming/DataOutputStream.h"
#include "data/constructs/rfile/meta/LocalityGroupMetaData.h"

namespace cclient {
namespace data {
MetaBlock::MetaBlock() {}

MetaBlock::~MetaBlock() {}

uint64_t MetaBlock::write(cclient::data::streams::DataOutputStream *outStream) {
  // write the magic number.
  outStream->writeInt(MAGIC_NUMBER);
  // write version of the RFIle
  outStream->writeInt(RFILE_VERSION);
  // write the size of the locaity groups.
  outStream->writeInt(localityGroups.size());
  uint64_t offset = 0;
  for (std::vector<LocalityGroupMetaData *>::iterator it =
           localityGroups.begin();
       it != localityGroups.end(); it++) {
    offset = (*it)->write(outStream);
  }
  return offset;
  // return outStream->getPos();
}
}  // namespace data
}  // namespace cclient