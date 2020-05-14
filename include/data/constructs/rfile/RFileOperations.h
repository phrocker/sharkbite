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

#ifndef RFILE_H_READER_
#define RFILE_H_READER_
#include <string>
#include <vector>
#include <iostream>
#include <fstream>
#include "RFile.h"
#include "SequentialRFile.h"
#include "data/iterators/MultiIterator.h"
#include "data/streaming/accumulo/KeyValueIterator.h"
#include "data/streaming/input/ReadAheadInputStream.h"
namespace cclient{

namespace data{

class RFileOperations{
public:
  static std::ifstream::pos_type filesize(const char* filename);
  static std::shared_ptr<cclient::data::SequentialRFile> openForWrite(const std::string &path);
  static cclient::data::RFile *open(const std::string &);
  static std::shared_ptr<cclient::data::SequentialRFile> openSequential(const std::string &);
  /**
   * Returns a merged read across the rfiles
   * @param rfiles absolute path to RFiles in HDFS or on the local file system
   * @return shared pointer to a multi iterator read
   */
  static std::shared_ptr<cclient::data::streams::KeyValueIterator> openManySequential(const std::vector<std::string>  &rfiles,int versions=0);

};
}
}
#endif
