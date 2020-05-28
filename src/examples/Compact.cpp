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
#include <fstream>
#include <iostream>
#include <sstream>

#include "../include/data/constructs/KeyValue.h"
#include "../include/data/constructs/security/Authorizations.h"
#include "../include/scanner/constructs/Results.h"
#include "../include/scanner/impl/Scanner.h"
#include "../include/data/constructs/client/zookeeperinstance.h"
#include "../include/interconnect/Master.h"
#include "../include/interconnect/tableOps/TableOperations.h"

#include "../include/data/constructs/rfile/SequentialRFile.h"
#include "../include/data/constructs/compressor/compressor.h"
#include "../include/data/constructs/compressor/zlibCompressor.h"
#include "../include/data/streaming/input/MemorymappedInputStream.h"
#include "data/streaming/input/ReadAheadInputStream.h"
#include "../include/data/streaming/OutputStream.h"
#include "data/iterators/DeletingMultiIterator.h"
#include "data/streaming/accumulo/KeyValueIterator.h"
#include "data/constructs/rfile/RFileOperations.h"
#include "data/streaming/input/HdfsInputStream.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"
#define BOOST_IOSTREAMS_NO_LIB 1

bool keyCompare(std::shared_ptr<cclient::data::KeyValue> a, std::shared_ptr<cclient::data::KeyValue> b) {
  return *(a->getKey()) < *(b->getKey());
}

std::ifstream::pos_type filesize(const char *filename) {
  std::ifstream in(filename, std::ifstream::ate | std::ifstream::binary);
  return in.tellg();
}

void compact(std::vector<std::string> &rfiles, uint32_t compressBlockSize, const std::string &file) {

  std::atomic<int64_t> cntr;
  cntr = 1;

  auto start = chrono::steady_clock::now();

  auto outStream = cclient::data::RFileOperations::write(file, compressBlockSize);

  std::shared_ptr<cclient::data::streams::KeyValueIterator> multi_iter = cclient::data::RFileOperations::openManySequential(rfiles);
  std::vector<std::string> cf;
  cclient::data::Range rng;

  cclient::data::security::Authorizations auths;

  cclient::data::streams::StreamSeekable seekable(rng, cf, auths, false);

  multi_iter->relocate(&seekable);
  long count = 0;
  uint64_t total_size = 0;
  outStream->addLocalityGroup();
  while (multi_iter->hasNext()) {

    outStream->append(std::make_shared<cclient::data::KeyValue>(multi_iter->getTopKey(), multi_iter->getTopValue()));

    multi_iter->next();
    count++;
    if ((count % 10000) == 0)
      std::cout << "Count is at " << count << std::endl;

  }
  outStream->close();

  auto end = chrono::steady_clock::now();

  std::cout << "we done at " << count << " " << chrono::duration_cast<chrono::milliseconds>(end - start).count() << std::endl;
  std::cout << "Skipped " << multi_iter->getEntriesFiltered() << std::endl;

  return;
}
int main(int argc, char **argv) {

  if (argc < 2) {
    std::cout << "Arguments required: ./RfileReadExample" << " -r <input rfile(s) can be more than one>  " << std::endl;
    std::cout << "Optional arguments:     -p -- print keys  " << std::endl;
    std::cout << "Optional arguments:     -v <visibility> -- visibility  " << std::endl;
    exit(1);
  }

  logging::LoggerConfiguration::enableTraceLogger();

  std::vector<std::string> rfiles;
  std::string outputfile;
  uint32_t compressBlockSize = 256 * 1024;

  if (argc >= 2) {
    for (int i = 1; i < argc; i++) {
      // always assume big endian
      std::string key = argv[i];

      if (key == "-o") {
        if (i + 1 < argc) {
          outputfile = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply visibility");
        }
      }

      if (key == "-s") {
        if (i + 1 < argc) {
          compressBlockSize = std::stoi(argv[i + 1]);
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply size");
        }
      }

      if (key == "-r") {
        if (i + 1 < argc) {
          rfiles.push_back(argv[i + 1]);
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply rfile");
        }
      }

    }
  }

  if (!rfiles.empty()) {
    compact(rfiles, compressBlockSize, outputfile);
  }

  return 0;
}

