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

#ifndef DATAINDEX_H_
#define DATAINDEX_H_

#include <vector>

#include "../../compressor/compressor.h"
#include "../../compressor/CompressorFactory.h"
#include "../../compressor/compression_algorithm.h"
#include "../../../streaming/DataOutputStream.h"
#include "../../../streaming/Streams.h"

#include "BlockRegion.h"
namespace cclient {
namespace data {
class DataIndex : public cclient::data::streams::StreamInterface {
 public:
  explicit DataIndex(cclient::data::compression::Compressor *compressor);
  DataIndex();
  ~DataIndex();

  void setCompressionAlgorithm(cclient::data::compression::Compressor *compressor)

  {
    compressionAlgorithm = compressor->getAlgorithm();
  }

  BlockRegion* addBlockRegion(BlockRegion region) {
    BlockRegion *reg = new BlockRegion(region);
    reg->setCompressor(cclient::data::compression::CompressorFactory::create( compressionAlgorithm ));
    listRegions.push_back(reg);
    return reg;
  }

  BlockRegion* addBlockRegion() {
    BlockRegion *reg = new BlockRegion();
    reg->setCompressor(cclient::data::compression::CompressorFactory::create( compressionAlgorithm ));
    listRegions.push_back(reg);
    return reg;
  }

  BlockRegion* getBlockRegion(int index) {
    return listRegions.at(index);
  }

  cclient::data::compression::CompressionAlgorithm getCompressionAlgorithm() {
    return compressionAlgorithm;
  }

  uint64_t read(cclient::data::streams::InputStream *in);

  uint64_t write(cclient::data::streams::OutputStream *out);

  DataIndex& operator=(const DataIndex &other) {
    compressionAlgorithm = other.compressionAlgorithm;
    listRegions.insert(listRegions.end(), other.listRegions.begin(), other.listRegions.end());
    return *this;
  }

 protected:
  std::vector<BlockRegion*> listRegions;
  cclient::data::compression::CompressionAlgorithm compressionAlgorithm;

};
}
}
#endif /* DATAINDEX_H_ */
