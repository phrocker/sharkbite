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

#ifndef META_INDEX_H_
#define META_INDEX_H_

#include <memory>
#include <vector>
#include <map>
#include <cassert>
#include <iostream>

#include "BlockRegion.h"

#include "../../compressor/algorithm.h"
#include "../../compressor/compression_algorithm.h"
#include "../../compressor/CompressorFactory.h"
#include "../../compressor/compressor.h"
#include "../../../streaming/Streams.h"
#include "../../../streaming/OutputStream.h"
#include "block_comp_stream.h"
#include <data/exceptions/ClientException.h>

namespace cclient {
namespace data {

class MetaIndexEntry : public cclient::data::streams::StreamInterface {
 public:
  MetaIndexEntry()
      :
      metaName("") {

  }

  virtual ~MetaIndexEntry() {

  }

  explicit MetaIndexEntry(std::unique_ptr<cclient::data::compression::Compressor> compressor)
      :
      metaName(""),
      comp(std::move(compressor)) {
    compressionAlgo = comp->getAlgorithm();
  }

  explicit MetaIndexEntry(cclient::data::streams::InputStream *in) {
    read(in);
  }

  MetaIndexEntry(std::string metameta, BlockRegion reg)
      :
      metaName(metameta),
      region(reg) {
    compressionAlgo = reg.getAlgorithm();
  }

  cclient::data::streams::InputStream*
  readDataStream(cclient::data::streams::InputStream *in) {
    uint64_t prevPosition = in->getPos();
    if (prevPosition < 0) {
      throw cclient::exceptions::ClientException("Invalid position in index block");
    }
    return new BlockCompressorStream(in, cclient::data::compression::CompressorFactory::create(compressionAlgo), &region);
  }

  uint64_t read(cclient::data::streams::InputStream *in) {

    std::string fullMetaName = in->readString();

    if (fullMetaName.at(0) == 'd' && fullMetaName.at(4) == ':') {
      metaName = fullMetaName.substr(5);
    } else {
      throw std::runtime_error("Corrupted Meta region Index , received " + fullMetaName);
    }
    auto z = in->readString();

    compressionAlgo = cclient::data::compression::CompressionAlgorithm(z);

    region.read(in);
    return in->getPos();

  }

  uint64_t write(cclient::data::streams::OutputStream *out) {

    std::string writeString = "data:";
    writeString.append(metaName);

    out->writeString(writeString);
    out->writeString(compressionAlgo.getName());
    return region.write(out);
  }

  std::string getMetaName() {

    return metaName;
  }

  MetaIndexEntry&
  operator=(const MetaIndexEntry &other) {
    metaName = other.metaName;
    compressionAlgo = other.compressionAlgo;
    region = other.region;

    if (other.comp != NULL) {

      comp = other.comp->newInstance();
    }

    return *this;
  }

  BlockRegion*
  getRegion() {
    return &region;
  }

  void setName(std::string name) {
    metaName = name;
  }

  void setAlgorithm(const cclient::data::compression::Algorithm &algo) {

    compressionAlgo = algo;
  }

  cclient::data::compression::CompressionAlgorithm getAlgorithm() {
    return compressionAlgo;
  }

  void setBlockRegion(BlockRegion reg) {

    region = reg;
  }

 protected:
  std::string metaName;
  cclient::data::compression::CompressionAlgorithm compressionAlgo;
  BlockRegion region;
  std::unique_ptr<cclient::data::compression::Compressor> comp;
};

/**
 * Meta Indices can be thought of as meta entries for all index entries
 * The index entries contain the BlockRegion, which are the compressed
 * regions for the particular entry we are describing.
 */
class MetaIndex : public cclient::data::streams::StreamInterface {
 public:

  MetaIndex();

  virtual ~MetaIndex();

  /**
   * Adds an entry to the Meta Index
   * @param indexEntry the Meta Index entry
   * @param out output stream, from which we gather the region
   * offset
   */
  void addEntry(const std::shared_ptr<MetaIndexEntry> &indexEntry, cclient::data::streams::DataOutputStream *out);

  /**
   * Returns the entry associated with the given name.
   * @param name name of entry we wish to retrieve
   * @returns MetaIndexEntry pointer. Not constant as we may
   * wish, and are allowed, to modify the index Entry. Note
   * that this may return null.
   */
  MetaIndexEntry* getEntry(const std::string &name);

  /**
   * Prepares a new MetaIndexEntry for this meta index table.
   * @param name name of index entry
   * @param comp compressor. not constant as this will eventually
   * be used for modifications and compression ( thus potentially
   * modifying the internals of the compressor )
   * @returns newly allocated MetaIndexEntry
   */
  MetaIndexEntry* prepareNewEntry(const std::string &name, std::unique_ptr<cclient::data::compression::Compressor> comp);

  uint64_t read(cclient::data::streams::InputStream *in);

  /**
   * @Override
   */
  uint64_t write(cclient::data::streams::OutputStream *out);

  std::map<std::string, std::shared_ptr<MetaIndexEntry>>* getEntries();

 protected:
  // meta index map. This will map the names of those
  // meta index entries to the entries.
  std::map<std::string, std::shared_ptr<MetaIndexEntry>> index;

};
}
}
#endif /* META_INDEX_H_ */
