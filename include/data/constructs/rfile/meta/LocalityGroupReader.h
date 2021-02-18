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
#ifndef INCLUDE_DATA_CONSTRUCTS_RFILE_META_LOCALITYGROUPREADER_H_
#define INCLUDE_DATA_CONSTRUCTS_RFILE_META_LOCALITYGROUPREADER_H_

#include <memory>
#include <future>

#include "data/streaming/accumulo/StreamSeekable.h"
#include "data/streaming/OutputStream.h"
#include "data/streaming/input/InputStream.h"
#include "data/streaming/Streams.h"
#include "data/streaming/StreamRelocation.h"
#include "data/streaming/StreamEnvironment.h"
#include "data/streaming/accumulo/FileIterator.h"
#include "data/streaming/DataStream.h"
#include "data/exceptions/IllegalArgumentException.h"
#include "data/exceptions/InterationInterruptedException.h"
#include "IndexEntry.h"

#include "../bcfile/BlockCompressedFile.h"
#include "data/extern/concurrentqueue/concurrentqueue.h"
#include "LocalityGroupMetaData.h"
// constructs

#include "data/constructs/Key.h"
#include "data/constructs/value.h"
#include "data/constructs/compressor/compressor.h"
#include "data/constructs/Range.h"
#include "data/constructs/SkippedRelativeKey.h"
#include "data/constructs/rkey.h"
#include "data/constructs/security/Authorizations.h"
#include "data/constructs/predicates/key/KeyPredicates.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"

namespace cclient {
namespace data {

struct ReadAheadProxy {
  std::unique_ptr<cclient::data::streams::InputStream> stream;
  std::atomic<uint32_t> entries;
  std::atomic<bool> checkRange;
  std::atomic<bool> finished;
  std::atomic<bool> hasData;
  std::atomic<bool> interrupt;
};

class LocalityGroupReader : public cclient::data::streams::FileIterator {
 private:
  std::shared_ptr<logging::Logger> logger;
 protected:
  cclient::data::ArrayAllocatorPool *allocatorInstance;
  BlockCompressedFile *bcFile;
  cclient::data::streams::InputStream *reader;
  int version;
  volatile bool closed;
  bool checkRange;
  volatile bool topExists = false;
  std::unique_ptr<cclient::data::streams::InputStream> currentStream;
  volatile bool interrupted = false;
  Range *currentRange;
  std::shared_ptr<SerializedIndex> iiter;
  std::shared_ptr<Key> prevKey;
  uint32_t entriesLeft;
  uint64_t entriesSkipped;
  std::shared_ptr<IndexManager> index;

  moodycamel::ConcurrentQueue<cclient::data::compression::Compressor*> compressors;
  moodycamel::ConcurrentQueue<std::vector<uint8_t>*> compressedBuffers;
  moodycamel::ConcurrentQueue<cclient::data::streams::ByteOutputStream*> outputBuffers;

  int blockCount;
  std::shared_ptr<Key> firstKey;
  int startBlock;

  std::shared_ptr<RelativeKey> rKey;

  std::shared_ptr<Value> val;

  bool readAheadEnabled;

  std::atomic<bool> readAheadRunning;

  std::condition_variable readAheadConsumerCondition;
  std::condition_variable readAheadCondition;
  std::mutex readAheadMutex;
  std::future<size_t> readAhead;
  ReadAheadProxy readAheadResult;
  cclient::data::security::Authorizations auths;
  std::shared_ptr<cclient::data::AgeOffEvaluator> ageoff_evaluator;
  std::shared_ptr<cclient::data::KeyPredicate> key_predicate;

  void startReadAhead();

  void close();

 public:
  explicit LocalityGroupReader(BlockCompressedFile *bcFile, cclient::data::streams::InputStream *input_stream, LocalityGroupMetaData *metadata, cclient::data::ArrayAllocatorPool *allocatorInstancePtr,int version)
      :
      logger(logging::LoggerFactory<LocalityGroupReader>::getLogger()),
      bcFile(bcFile),
      reader(input_stream),
      version(version),
      closed(false),
      checkRange(false),
      topExists(false),
      interrupted(false),
      currentRange(NULL),
      iiter(NULL),
      prevKey(
      NULL),
      readAheadEnabled(false),
      readAheadRunning(false),
      entriesLeft(-1),
      entriesSkipped(0),
      allocatorInstance(allocatorInstancePtr) {
    index = metadata->getIndexManager();
    firstKey = std::dynamic_pointer_cast<Key>(metadata->getFirstKey());
    startBlock = metadata->getStartBlock();
    blockCount = index->getSize();
    rKey = NULL;
  }

  virtual ~LocalityGroupReader() {
    close();
  }

  void enableReadAhead() {
    readAheadEnabled = true;
  }

  std::shared_ptr<Key> getFirstKey() {
    return firstKey;
  }

  uint64_t getEntriesFiltered() const {
    return entriesSkipped;
  }

  std::shared_ptr<Key> getLastKey() {
    return iiter->get(iiter->size() - 1)->getKey();
  }

  std::shared_ptr<Key> getTopKey() {
    return rKey->getKey();
  }

  std::shared_ptr<Value> getTopValue() {
    return val;
  }

  void limitVisibility(cclient::data::security::Authorizations *auths) {
    this->auths = *auths;
  }

  void setAgeOff(const std::shared_ptr<cclient::data::AgeOffEvaluator> &ageoff_evaluator){
    this->ageoff_evaluator=ageoff_evaluator;
  }

  void setKeyPredicate(const std::shared_ptr<cclient::data::KeyPredicate> &predicate){
    this->key_predicate=predicate;
  }

  bool hasTop() {
    return topExists;
  }

  void seek(cclient::data::streams::StreamRelocation *position);

  std::vector<std::shared_ptr<cclient::data::Key>> getBlockKeys(cclient::data::streams::StreamRelocation *position);

  void next(bool errorOnNext=true);

  std::unique_ptr<cclient::data::streams::InputStream> getDataBlock(uint32_t index);

  std::unique_ptr<cclient::data::streams::InputStream> getDataBlock(uint64_t offset, uint64_t compressedSize, uint64_t rawSize, bool use_cached = false);

}

;
}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_LOCALITYGROUPREADER_H_ */

