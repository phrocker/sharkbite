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

#include "data/constructs/rfile/meta/LocalityGroupMetaData.h"

#include "data/constructs/Key.h"
#include "data/constructs/rfile/meta/../../../streaming/DataOutputStream.h"
namespace cclient {
namespace data {

LocalityGroupMetaData::LocalityGroupMetaData(uint32_t startBlockVal,
                                             std::string name)
    : startBlock(startBlockVal),
      firstKey(NULL),
      isDefaultLG(false),
      read_version(4),
      indexManager(NULL) {
  this->name = name;
  if (name == "") {
    isDefaultLG = true;
  } else {
    isDefaultLG = false;
  }
}

LocalityGroupMetaData::LocalityGroupMetaData(
    std::unique_ptr<cclient::data::compression::Compressor> compressorRef,
    int version, cclient::data::streams::InputStream *reader)
    : firstKey(NULL),
      isDefaultLG(false),
      read_version(version),
      compressorRef(std::move(compressorRef)) {
  indexManager = std::make_shared<IndexManager>(
      this->compressorRef->newInstance(), reader, version);
}

LocalityGroupMetaData::~LocalityGroupMetaData() {
  for (auto pair : columnFamilies) {
    delete[] pair.first.first;
  }
}
/**
 read function for the Locality Meta Data
 @param outStream output stream.
 @return position of output stream.
 **/
uint64_t LocalityGroupMetaData::read(cclient::data::streams::InputStream *in) {
  isDefaultLG = in->readBoolean();
  if (!isDefaultLG) {
    name = in->readUTF();
  }

  if (read_version == 3 || read_version == 4 || read_version == 6 ||
      read_version == 7)
    startBlock = in->readInt();

  int size = in->readInt();

  if (size == -1) {
    if (!isDefaultLG) throw std::runtime_error("Non default LG");

  } else {
    columnFamilies.clear();
    for (int32_t i = 0; i < size; i++) {
      int len = in->readInt();
      uint8_t *cf = new uint8_t[len];
      in->readBytes(cf, len);
      uint64_t count = in->readLong();
      columnFamilies.insert(std::make_pair(std::make_pair(cf, len), count));
    }
  }

  if (in->readBoolean()) {
    firstKey = std::make_shared<Key>();

    firstKey->read(in);
  } else {
    firstKey = NULL;
  }

  indexManager->read(in);

  return in->getPos();
}

/**
 write function for the Locality Meta Data
 @param outStream output stream.
 @return position of output stream.
 **/
uint64_t LocalityGroupMetaData::write(
    cclient::data::streams::DataOutputStream *outStream) {
  outStream->writeBoolean(isDefaultLG);
  if (!isDefaultLG) outStream->writeString(name);

  outStream->writeInt(startBlock);
  // write a -1 indicating that we're writing
  // a default locality group.
  if (isDefaultLG) {
    outStream->writeInt(-1);

  } else {
    outStream->writeInt(columnFamilies.size());
    // now write out columnFamilies
  }
  bool haveKey = (firstKey != NULL);
  outStream->writeBoolean(haveKey);
  if (haveKey) firstKey->write(outStream);
  // retrieved the encoded indices.
  std::pair<char *, size_t> indices = buildIndexArray();

  outStream->writeInt(offsets.size());

  for (std::vector<int>::iterator it = offsets.begin(); it != offsets.end();
       ++it) {
    outStream->writeInt((*it));
  }

  // write out the number of indices.
  outStream->writeInt(indices.second);

  uint64_t pos =
      outStream->writeBytes((const uint8_t *)indices.first, indices.second);

  delete[] indices.first;

  return pos;
}
}  // namespace data
}  // namespace cclient
