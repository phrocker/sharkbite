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

#include "data/constructs/rfile/RFile.h"

#include "data/streaming/input/NetworkOrderInputStream.h"

namespace cclient {
namespace data {

RFile::RFile(streams::OutputStream *output_stream,
             std::unique_ptr<BlockCompressedFile> bWriter)
    : out_stream(output_stream),
      blockWriter(std::move(bWriter)),
      dataClosed(false),
      currentBlockWriter(NULL),
      closed(false),
      dataBlockCnt(0),
      currentBlockStart(0),
      entries(0),
      currentLocalityGroup(NULL),
      max_size(128 * 1024),
      in_stream(nullptr),
      myInputStream(nullptr),
      currentLocalityGroupReader(nullptr) {
  if (output_stream == NULL || blockWriter == NULL) {
    throw std::runtime_error(
        "Output Stream and BC File Writer should not be NULL");
  }

  compressorRef = blockWriter->cloneCompressor();

  maxBlockSize = compressorRef->getBufferSize() * 8;

  myDataStream = output_stream;

  lastKeyValue = NULL;
}

RFile::RFile(streams::InputStream *input_stream, long fileLength)
    : in_stream(input_stream),
      dataClosed(false),
      currentBlockWriter(NULL),
      closed(false),
      dataBlockCnt(0),
      currentBlockStart(0),
      entries(0),
      currentLocalityGroup(NULL) {
  if (input_stream == NULL) {
    throw std::runtime_error(
        "InputSTream Stream and BC Reader Writer should not be NULL");
  }

  maxBlockSize = 128 * 1024;

  myInputStream = input_stream;

  lastKeyValue = NULL;

  blockWriter = std::make_unique<BlockCompressedFile>(in_stream, fileLength);

  compressorRef = blockWriter->cloneCompressor();

  maxBlockSize = compressorRef->getBufferSize() * 8;

  streams::InputStream *metaBlock = blockWriter->getMetaIndex()
                                        ->getEntry("RFile.index")
                                        ->readDataStream(in_stream);

  readLocalityGroups(metaBlock);

  delete metaBlock;
}

bool RFile::hasNext() { return currentLocalityGroupReader->hasTop(); }

void RFile::readLocalityGroups(streams::InputStream *metaBlock) {
  int magic = metaBlock->readInt();

  int version = metaBlock->readInt();

  if (magic != MAGIC_NUMBER && magic != MAGIC_NUMBER2) {
    throw std::runtime_error("Did not see expected magic number. Saw " +
                             std::to_string(magic));
  }

  switch (version) {
    case RFILE_VERSION:
    case RFILE_VERSION_3:
    case RFILE_VERSION_6:
    case RFILE_VERSION_7:
    case RFILE_VERSION_8:
      break;
    default:
      throw std::runtime_error("Did not see expected magic number");
  };

  int size = metaBlock->readInt();

  localityGroups.resize(size);

  for (int i = 0; i < size; i++) {
    auto meatadata = std::make_shared<LocalityGroupMetaData>(
        compressorRef->newInstance(), version, in_stream);
    meatadata->read(metaBlock);
    localityGroups.push_back(meatadata);
    localityGroupReaders.push_back(std::make_shared<LocalityGroupReader>(
        blockWriter.get(), in_stream, meatadata, &allocatorInstance, version));
  }

  currentLocalityGroupReader = localityGroupReaders.front();
}

void RFile::next() { currentLocalityGroupReader->next(); }

cclient::data::streams::DataStream<
    std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>>>
    *RFile::operator++() {
  currentLocalityGroupReader->next();
  return this;
}

std::shared_ptr<Key> RFile::getTopKey() {
  return currentLocalityGroupReader->getTopKey();
}

std::shared_ptr<Value> RFile::getTopValue() {
  return currentLocalityGroupReader->getTopValue();
}

std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>> RFile::operator*() {
  return std::make_pair(currentLocalityGroupReader->getTopKey(),
                        currentLocalityGroupReader->getTopValue());
}

std::shared_ptr<cclient::data::KeyValue> RFile::getTop() {
  return std::make_shared<cclient::data::KeyValue>(
      currentLocalityGroupReader->getTopKey(),
      currentLocalityGroupReader->getTopValue());
}

RFile::~RFile() {}

bool RFile::append(std::shared_ptr<KeyValue> kv) {
  if (dataClosed || closed)
    throw std::runtime_error("Appending data failed, data block closed");

  if (currentLocalityGroup == nullptr) {
    addLocalityGroup();  // add the default locality group.
  }

  if (currentLocalityGroup->getFirstKey() == NULL) {
    std::shared_ptr<StreamInterface> firstKey = kv->getKey()->getStream();
    // set the first key for the current locality group.
    setCurrentLocalityKey(firstKey);
  }

  std::shared_ptr<Key> prevKey = NULL;
  if (NULL != lastKeyValue) {
    prevKey = lastKeyValue->getKey();
  }
  RelativeKey key(prevKey, kv->getKey(), ArrayAllocatorPool::getInstance());

  if (NULL == currentBlockWriter) {
    currentBlockWriter =
        (BlockCompressorStream *)blockWriter->createDataStream(myDataStream);
    currentBlockStart = currentBlockWriter->getPos();
    currentBlockCount = 0;
  }

  entries++;
  currentBlockCount++;
  key.write(currentBlockWriter);
  uint64_t position = kv->getValue()->write(currentBlockWriter);

  lastKeyValue = kv;

  // we've written all we can write doctor.
  if (position - currentBlockStart >= maxBlockSize) {
    currentBlockWriter->flush();
    closeBlock(kv->getKey());

    delete currentBlockWriter;
    currentBlockWriter = NULL;
  }

  return true;
}

bool RFile::append(
    std::vector<std::shared_ptr<streams::StreamInterface>> *keyValues,
    uint32_t average_recordSize, bool isSorted) {
  if (dataClosed || closed)
    throw std::runtime_error("Appending data failed, data block closed");

  if (keyValues == NULL || keyValues->size() == 0) return false;

  uint32_t recordIncrement = (maxBlockSize / average_recordSize);

  std::shared_ptr<cclient::data::Key> key;
  std::shared_ptr<cclient::data::Key> firstKey = NULL;
  firstKey = std::static_pointer_cast<cclient::data::KeyValue>(keyValues->at(0))
                 ->getKey();
  ;
  // set the first key for the current locality group.
  setCurrentLocalityKey(firstKey);

  uint64_t j = 0;
  for (uint64_t i = 0; i < keyValues->size(); i += (j - i)) {
    currentBlockWriter =
        (BlockCompressorStream *)blockWriter->createDataStream(myDataStream);
    for (j = i; j < keyValues->size() && j < (i + recordIncrement); j++) {
      keyValues->at(j)->write(currentBlockWriter);
      entries++;
    }
    currentBlockWriter->flush();
    auto ky =
        std::static_pointer_cast<cclient::data::KeyValue>(keyValues->at(j - 1))
            ->getKey();
    closeBlock(ky);

    delete currentBlockWriter;
    currentBlockWriter = NULL;
  }

  if (currentBlockWriter != NULL) {
    currentBlockWriter->flush();
    delete currentBlockWriter;
  }
  key = std::static_pointer_cast<cclient::data::KeyValue>(keyValues->back())
            ->getKey();
  closeBlock(key);

  return true;
}

void RFile::close() {
  closeData();

  // create  new compression stream.

  BlockCompressorStream *outStream =
      (BlockCompressorStream *)blockWriter->createCompressorStream(
          myDataStream, blockWriter->prepareNewEntry("RFile.index"));
  // prepare the RFile Index.

  MetaBlock block;
  closeCurrentGroup();
  block.addLocalityGroups(localityGroups);
  block.write(outStream);
  outStream->flush();
  blockWriter->write(myDataStream);
  blockWriter->close();

  delete outStream;
  closed = true;
}
}  // namespace data
}  // namespace cclient
