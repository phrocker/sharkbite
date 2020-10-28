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

#include "data/constructs/rfile/ShardedRFile.h"


namespace cclient {
namespace data {

ShardedRFile::ShardedRFile(streams::OutputStream *output_stream,
                                 std::unique_ptr<BlockCompressedFile> bWriter)
    : SequentialRFile(output_stream,std::move(bWriter)) {
}
ShardedRFile::ShardedRFile(
    std::unique_ptr<streams::OutputStream> output_stream,
    std::unique_ptr<BlockCompressedFile> bWriter)
    : SequentialRFile(std::move(output_stream),std::move(bWriter)) {
}

ShardedRFile::ShardedRFile(
    std::unique_ptr<streams::InputStream> input_stream, long fileLength)
    : SequentialRFile(std::move(input_stream),fileLength) {
}

ShardedRFile::ShardedRFile(streams::InputStream *input_stream,
                                 long fileLength)
    : SequentialRFile(input_stream,fileLength) {
}

ShardedRFile::~ShardedRFile(){
}

}  // namespace data
}  // namespace cclient