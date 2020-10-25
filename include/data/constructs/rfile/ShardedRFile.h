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

#pragma once

#include <vector>
#include <future>
#include <deque>
#include <iterator>
#include <memory>

#include "../compressor/compressor.h"
#include "../../streaming/Streams.h"
#include "../../streaming/accumulo/KeyValueIterator.h"
#include "../../streaming/input/InputStream.h"
#include "../KeyValue.h"
// meta
#include "meta/MetaBlock.h"
#include "meta/LocalityGroupReader.h"

// bcfile
#include "bcfile/meta_index.h"
#include "bcfile/data_index.h"
#include "bcfile/BlockCompressedFile.h"
#include "SequentialRFile.h"

namespace cclient {
namespace data {

class ShardedRFile : public cclient::data::SequentialRFile {

 public:
  /**
   Constructor
   @param output_stream output stream for the r file
   @param bWriter block compressed writer.
   @param blockSize the desired block size of the bc file.
   **/
  explicit ShardedRFile(cclient::data::streams::OutputStream *output_stream, std::unique_ptr<BlockCompressedFile> bWriter);

  explicit ShardedRFile(std::unique_ptr<cclient::data::streams::OutputStream> output_stream, std::unique_ptr<BlockCompressedFile> bWriter);

  /**
   Constructor
   @param input stream output stream for the r file
   @param bWriter block compressed writer.
   @param blockSize the desired block size of the bc file.
   **/
  explicit ShardedRFile(cclient::data::streams::InputStream *input_stream, long fileLength);

  /**
   Constructor
   @param input stream output stream for the r file
   @param bWriter block compressed writer.
   @param blockSize the desired block size of the bc file.
   **/
  explicit ShardedRFile(std::unique_ptr<cclient::data::streams::InputStream> input_stream, long fileLength);

  ShardedRFile(ShardedRFile &&other) = default;

  ShardedRFile& operator=(ShardedRFile &&other) = default;

  virtual
  ~ShardedRFile();


};
}
}
