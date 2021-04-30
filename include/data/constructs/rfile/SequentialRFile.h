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

#ifndef SEQUENTIAL_RFILE_H_
#define SEQUENTIAL_RFILE_H_

#include <vector>
#include <future>
#include <deque>
#include <iterator>
#include <memory>
#include "data/constructs/predicates/key/KeyPredicates.h"
#include "../compressor/compressor.h"
#include "../../streaming/Streams.h"
#include "../../streaming/accumulo/KeyValueIterator.h"
#include "../../streaming/input/InputStream.h"
#include "data/iterators/HeapIterator.h"
#include "../KeyValue.h"
#include "meta/LocalityGroupIterator.h"
// meta
#include "meta/MetaBlock.h"
#include "meta/LocalityGroupReader.h"

// bcfile
#include "bcfile/meta_index.h"
#include "bcfile/data_index.h"
#include "bcfile/BlockCompressedFile.h"

namespace cclient {
namespace data {

class SequentialRFile : public cclient::data::HeapIterator, public std::enable_shared_from_this<SequentialRFile> {

 public:
  /**
   Constructor
   @param output_stream output stream for the r file
   @param bWriter block compressed writer.
   @param blockSize the desired block size of the bc file.
   **/
  explicit SequentialRFile(cclient::data::streams::OutputStream *output_stream, std::unique_ptr<BlockCompressedFile> bWriter);

  explicit SequentialRFile(std::unique_ptr<cclient::data::streams::OutputStream> output_stream, std::unique_ptr<BlockCompressedFile> bWriter);

  /**
   Constructor
   @param input stream output stream for the r file
   @param bWriter block compressed writer.
   @param blockSize the desired block size of the bc file.
   **/
  explicit SequentialRFile(cclient::data::streams::InputStream *input_stream, long fileLength);

  /**
   Constructor
   @param input stream output stream for the r file
   @param bWriter block compressed writer.
   @param blockSize the desired block size of the bc file.
   **/
  explicit SequentialRFile(std::unique_ptr<cclient::data::streams::InputStream> input_stream, long fileLength);

  SequentialRFile(SequentialRFile &&other) = default;

  SequentialRFile& operator=(SequentialRFile &&other) = default;

  virtual
  ~SequentialRFile();

  /**
   Get the maximum block size.
   @return block size.
   */
  uint32_t getMaxBlockSize() {
    return maxBlockSize;
  }

  std::string colvis;

  void limitVisibility(const std::string &viz) {
    colvis = viz;
  }

  /**
   Gets the pointer to the number of entries.
   @Deprecated. this isn't needed!
   @return pointer to entries.
   **/
  uint32_t* getEntryPtr() {
    return &entries;
  }

  void addStreams(const std::vector<cclient::data::streams::OutputStream*> &streams) {
    std::copy(streams.begin(), streams.end(), std::back_inserter(ownedStreams));
  }

  /**
   Creates a data stream from the bc file strema.
   @return pointer to new stream.
   **/
  BlockCompressorStream* createStream() {
    return (BlockCompressorStream*) blockWriter->createDataStream(myDataStream);
  }

  /**
   Sets the first key for the current locality group.
   @param key incoming key to set.

   */
  void setCurrentLocalityKey(std::shared_ptr<StreamInterface> key) {
    if (key == NULL) {
      throw std::runtime_error("Key should not be null.");
    }
    currentLocalityGroup->setFirstKey(key);
  }

  static uint32_t generate_average(std::vector<std::shared_ptr<StreamInterface> > *keyValues) {
    cclient::data::streams::ByteOutputStream outStream(1024 * 1024);
    for (int i = 0; i < 10; i++) {
      keyValues->at(i)->write(&outStream);
    }

    uint32_t average = outStream.getPos() / 10;

    return average;

  }

  bool append(std::vector<std::shared_ptr<StreamInterface> > *keyValues, bool isSorted = false) {
    return append(keyValues, generate_average(keyValues), isSorted);
  }

  bool append(std::shared_ptr<KeyValue> kv);

  bool
  append(std::vector<std::shared_ptr<StreamInterface> > *keyValues, uint32_t average_recordSize, bool isSorted);

  /**
   Add a locality group
   @param name name of locality group. If it is empty, then we have a default
   locality group.
   **/
  void addLocalityGroup(std::string name = "") {
    closeCurrentGroup();
    auto group = std::make_shared<LocalityGroupMetaData>(dataBlockCnt, name);
    currentLocalityGroup = group;
    dataBlockCnt++;

  }
  /**
   Closes the RFile.
   **/
  void close();
  /**
   Closes the data section of the RFile.
   **/
  void closeData() {
    if (dataClosed)
      return;

    if (currentBlockWriter != NULL) {
      currentBlockWriter->flush();
      closeBlock(lastKeyValue->getKey());
      delete currentBlockWriter;
      currentBlockWriter = NULL;
    }

    dataClosed = true;

  }
  /**
   Closes a block, setting the last key before closing it
   and adding that entry to the current locality group.
   @param lastKey last key for locality group.
   **/
  void closeBlock(std::shared_ptr<cclient::data::Key> lastKey) {
    currentLocalityGroup->addIndexEntry(IndexEntry(lastKey, entries));
    dataBlockCnt = 0;
    entries = 0;
    currentBlockCount = 0;

  }

  void relocate(cclient::data::streams::StreamRelocation *location) {
    lgCache = cclient::data::LocalityGroupIterator::relocate(shared_from_this(),lgContext.get(),location,lgCache);

  }

  void setAgeOff(const std::shared_ptr<cclient::data::AgeOffEvaluator> &evaluator){
    ageoff_evaluator=evaluator;
    for(auto rdr : localityGroupReaders){
      auto map = std::dynamic_pointer_cast<cclient::data::LocalityGroupReader>(rdr);
      if (nullptr != map)
        map->setAgeOff(ageoff_evaluator);      
    }
  }

  void setKeyPredicate(const std::shared_ptr<cclient::data::KeyPredicate> &predicate){
    key_predicate=predicate;
    for(auto rdr : localityGroupReaders){
      auto map = std::dynamic_pointer_cast<cclient::data::LocalityGroupReader>(rdr);
      if (nullptr != map)
        map->setKeyPredicate(key_predicate);      
    }
  }

  std::vector<std::shared_ptr<cclient::data::Key>> getBlocks(cclient::data::streams::StreamRelocation *location);

  virtual DataStream<std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>>>* operator++() override;

  friend inline std::ostream&
  operator <<(std::ostream &out, SequentialRFile &rhs) {
    auto entries = rhs.blockWriter->getMetaIndex()->getEntries();

    for (std::map<std::string, std::shared_ptr<MetaIndexEntry>>::iterator it = entries->begin(); it != entries->end(); it++) {
      out << "Meta Block  : " << (*it).first << std::endl;
      auto entry = ((*it).second);
      out << "  Raw size  :" << entry->getRegion()->getRawSize() << std::endl;
      out << "  Compressed size :" << entry->getRegion()->getCompressedSize() << std::endl;
    }

    return out;
  }

  friend inline std::ostream&
  operator <<(std::ostream &out, SequentialRFile *rhs) {
    return operator<<(out, *rhs);
  }

  virtual std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>> operator*();


  virtual uint64_t getEntriesFiltered() {
    //currentLocalityGroupReader != nullptr ? entriesSkipped + currentLocalityGroupReader->getEntriesFiltered() :
    return  entriesSkipped;
  }

 protected:

  void
  readLocalityGroups(cclient::data::streams::InputStream *metaBlock);

  /**
   Closes the current locality group.
   **/
  void closeCurrentGroup() {
    if (currentLocalityGroup != NULL) {
      localityGroups.push_back(currentLocalityGroup);
      currentLocalityGroup = NULL;
      dataBlockCnt = 0;
      entries = 0;
    }

  }

  std::unique_ptr<cclient::data::streams::InputStream> ownedStream;
  std::unique_ptr<cclient::data::streams::OutputStream> ownedOutStream;

  std::vector<cclient::data::streams::OutputStream*> ownedStreams;

  // current locality group.
  std::shared_ptr<LocalityGroupMetaData> currentLocalityGroup;
  LocalityGroupReader *currentLocalityGroupReader;
  // number of entries in current block.
  uint32_t entries;
  uint32_t currentBlockCount;
  uint32_t currentBlockStart;
  uint64_t entriesSkipped;
  std::shared_ptr<KeyValue> lastKeyValue;
  // current data block count in locality group.
  uint16_t dataBlockCnt;
  // output stream
  cclient::data::streams::OutputStream *out_stream;
  // data output stream.
  cclient::data::streams::OutputStream *myDataStream;

  cclient::data::streams::InputStream *myInputStream;

  // list of locality group pointers.
  std::vector<std::shared_ptr<LocalityGroupMetaData>> localityGroups;
  std::vector<std::shared_ptr<cclient::data::LocalityGroup>> localityGroupReaders;
  std::shared_ptr<cclient::data::AgeOffEvaluator> ageoff_evaluator;
  std::shared_ptr<cclient::data::KeyPredicate> key_predicate;

  // block compressed file.
  std::unique_ptr<BlockCompressedFile> blockWriter;
  // compressor reference.
  std::unique_ptr<cclient::data::compression::Compressor> compressorRef;
  // current block writer, created from blockWriter.
  BlockCompressorStream *currentBlockWriter;
  // maximum block size.
  uint32_t maxBlockSize;
  // boolean identifying a closed data block.
  bool dataClosed;
  // boolean identifying closed rfile.
  bool closed;

  // primarily for reading
  cclient::data::streams::InputStream *in_stream;

  std::vector<cclient::data::ArrayAllocatorPool*> allocatorInstance;

  std::shared_ptr<cclient::data::LocalityGroupSeekCache> lgCache;
  std::unique_ptr<cclient::data::LocalityGroupContext> lgContext;

};
}
}
#endif /* RFILE_H_ */
