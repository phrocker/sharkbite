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

#include "data/constructs/rfile/bcfile/data_index.h"
namespace cclient {
namespace data {

DataIndex::DataIndex(cclient::data::compression::Compressor *compressor) {
  compressionAlgorithm = compressor->getAlgorithm();
}

DataIndex::DataIndex() {
}

DataIndex::~DataIndex() {
  for (std::vector<BlockRegion*>::iterator it = listRegions.begin(); it != listRegions.end(); it++)
    delete (*it);
}

uint64_t DataIndex::read(cclient::data::streams::InputStream *in) {
  compressionAlgorithm = cclient::data::compression::CompressionAlgorithm(in->readString());
  // TODO was encoded long
  uint64_t count = in->readHadoopLong();

  for (uint64_t i = 0; i < count; i++) {
    listRegions.push_back(new BlockRegion(in));
  }

  return in->getPos();
}

uint64_t DataIndex::write(cclient::data::streams::OutputStream *out) {

  out->writeString(compressionAlgorithm.getName());
  // TODO was encoded long
  out->writeEncodedLong(listRegions.size());
  for (std::vector<BlockRegion*>::iterator it = listRegions.begin(); it != listRegions.end(); it++) {
    (*it)->write(out);
  }

  return out->getPos();
}

}
}
