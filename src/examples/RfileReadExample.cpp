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
#include "data/iterators/MultiIterator.h"
#include "data/streaming/accumulo/KeyValueIterator.h"

#define BOOST_IOSTREAMS_NO_LIB 1

bool keyCompare(std::shared_ptr<cclient::data::KeyValue> a, std::shared_ptr<cclient::data::KeyValue> b) {
  return *(a->getKey()) < *(b->getKey());
}

std::ifstream::pos_type filesize(const char *filename) {
  std::ifstream in(filename, std::ifstream::ate | std::ifstream::binary);
  return in.tellg();
}
/*
void timer_start(std::atomic<int64_t> *biggie, unsigned int interval) {

  std::thread([interval, biggie]() {
    int64_t time = 1;
    std::this_thread::sleep_for(std::chrono::milliseconds(interval));
    while (true) {

      std::cout << "Rate is " << (biggie->load() / time) << " keys per ms " << std::endl;
      std::this_thread::sleep_for(std::chrono::milliseconds(interval));
      time += interval;
    }
  }).detach();
}
*/
std::unique_ptr<cclient::data::streams::KeyValueIterator> createMultiReader(std::vector<std::string> rfiles) {

  std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>> iters;
  for (const auto &path : rfiles) {
    std::fstream::pos_type size = filesize(path.c_str());
    auto in = std::make_unique<std::ifstream>(path, std::ifstream::ate | std::ifstream::binary);

    auto stream = std::make_unique<cclient::data::streams::InputStream>(std::move(in), 0);

    auto endstream = std::make_unique<cclient::data::streams::ReadAheadInputStream>(std::move(stream), 128 * 1024, 1024 * 1024, size);

    if (rfiles.size() == 1) {
      return std::move(std::make_unique<cclient::data::SequentialRFile>(std::move(endstream), size));
    } else {
      auto newRFile = std::make_shared<cclient::data::SequentialRFile>(std::move(endstream), size);
      iters.emplace_back(newRFile);
    }

  }
  return std::move(std::make_unique<cclient::data::MultiIterator>(iters));
}

void readRfile(std::vector<std::string> &rfiles, uint16_t port, bool print, const std::string &visibility) {

  std::atomic<int64_t> cntr;
  cntr = 1;

  auto start = chrono::steady_clock::now();
 
  std::unique_ptr<cclient::data::streams::KeyValueIterator> multi_iter = createMultiReader(rfiles);
  std::vector<std::string> cf;
  cclient::data::Range rng;

  cclient::data::security::Authorizations auths;
  if (!visibility.empty()){
    auths.addAuthorization(visibility);
  }

  cclient::data::streams::StreamSeekable seekable(rng, cf,auths, false);

  multi_iter->relocate(&seekable);
  long count = 0;
  uint64_t total_size = 0;
  while (multi_iter->hasNext()) {

    if (print) {
      std::cout << "has next " << (**multi_iter).first << std::endl;
      std::stringstream ss;
      ss << (**multi_iter).first;
      total_size += ss.str().size();
    }

    multi_iter->next();

    count++;
    if ((count % 100000) == 0)
      cntr.fetch_add(100000, std::memory_order_relaxed);

  }

  if (print) {
    std::cout << "Bytes accessed " << total_size << std::endl;
  }
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

  std::vector<std::string> rfiles;
  std::string visibility;
  bool print = false;

  if (argc >= 2) {
    for (int i = 1; i < argc; i++) {
      // always assume big endian
      std::string key = argv[i];
      if (key == "-p") {
        print = true;
      }

      if (key == "-v") {
        if (i + 1 < argc) {
          visibility = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply visibility");
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
    readRfile(rfiles, 0, print, visibility);
  }

  return 0;
}

