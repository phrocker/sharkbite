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
#include "../include/interconnect/Accumulo.h"
#include "../include/interconnect/tableOps/TableOperations.h"

#include "../include/data/constructs/rfile/SequentialRFile.h"
#include "../include/data/constructs/compressor/compressor.h"
#include "../include/data/constructs/compressor/zlibCompressor.h"
#include "../include/data/streaming/input/MemorymappedInputStream.h"
#include "data/streaming/input/ReadAheadInputStream.h"
#include "../include/data/streaming/OutputStream.h"
#include "data/iterators/MultiIterator.h"
#include "data/streaming/accumulo/KeyValueIterator.h"
#include "data/constructs/rfile/RFileOperations.h"
#include "data/streaming/input/HdfsInputStream.h"
#include "logging/Logger.h"
#include "data/constructs/predicates/key/KeyPredicates.h"
#include "logging/LoggerConfiguration.h"
#include "data/extern/knnspeed/avxmem.h"
#include "data/constructs/Text.h"
#include <signal.h>
#define BOOST_IOSTREAMS_NO_LIB 1

class KeySkipper : public cclient::data::KeyPredicate{
  private:
  size_t keyCount;
  size_t skipCount;
  size_t total_size;
  public:
  explicit KeySkipper( size_t keys_to_skip) : skipCount(keys_to_skip), total_size(0),keyCount(0){
  }

  virtual bool accept(const std::shared_ptr<cclient::data::Key> &key) override{
    if (++keyCount < skipCount){
      total_size+=key->length();
      return false;
    }else{
      keyCount = 0;
      return true;
    }
  }

  size_t getSkippedLength() const {
    return total_size;
  }
};

class RowPredicate : public cclient::data::KeyPredicate{
  private:
  std::string rowstring;
  public:
  explicit RowPredicate( const std::string &str) : rowstring(str){
  }

    virtual bool acceptRow(const std::shared_ptr<cclient::data::Text> &row) override{
        if (row->equals(rowstring.data(),rowstring.size())){
      return true;
    }
    return false;
    }


  virtual bool accept(const std::shared_ptr<cclient::data::Key> &key) override{
    if (key->getRowStr().find(rowstring) != std::string::npos){
      return true;
    }
    return false;
  }
};

class ColumnQualifierPredicate : public cclient::data::KeyPredicate{
  private:
  std::string cqstring;
  public:
  explicit ColumnQualifierPredicate( const std::string &str) : cqstring(str){
  }

  virtual bool accept(const std::shared_ptr<cclient::data::Key> &key) override{
    if (key->getColQualifierStr().find(cqstring) != std::string::npos){
      return true;
    }
    return false;
  }
};

bool keyCompare(std::shared_ptr<cclient::data::KeyValue> a, std::shared_ptr<cclient::data::KeyValue> b) {
  return *(a->getKey()) < *(b->getKey());
}

std::ifstream::pos_type filesize(const char *filename) {
  std::ifstream in(filename, std::ifstream::ate | std::ifstream::binary);
  return in.tellg();
}

volatile sig_atomic_t stop;

void inthand(int signum) {
  std::cout << "boyyyyy" << std::endl;
    stop = 1;
}


void readRfile(std::vector<std::string> &rfiles, uint16_t port,int threads, bool print,bool size, const std::string &visibility, const std::shared_ptr<cclient::data::KeyPredicate> &predicate,
std::string cq_nonpredicate) {

  stop = 0;
  std::atomic<int64_t> cntr;
  cntr = 1;
  
  signal(SIGINT, inthand);



  std::shared_ptr<cclient::data::streams::KeyValueIterator> multi_iter = nullptr;
  std::vector<std::string> cf;

  cclient::data::Range rng; // (key,false);

  if (threads > 1 && (rng.getInfiniteStartKey() && rng.getInfiniteStopKey())){
    multi_iter = cclient::data::RFileOperations::openParallelRFiles(rfiles,predicate);
  }
  else{
    multi_iter = cclient::data::RFileOperations::openManySequentialWithPredicate(rfiles,predicate);
  }
  

  cclient::data::security::Authorizations auths; 
  if (!visibility.empty()) {
    auths.addAuthorization(visibility);
  }

  auto seekable = std::make_shared<cclient::data::streams::StreamSeekable>(rng, cf, auths, false,10000);
  if (threads > 1 && (rng.getInfiniteStartKey() && rng.getInfiniteStopKey())){
    seekable->setThreads(threads);
  }
  multi_iter->relocate(seekable);
  long count = 0;
  uint64_t total_size = 0;
  // break with control+c 
  std::map<std::string,long> fi_counts;
  auto start = chrono::steady_clock::now();
  while (!stop && multi_iter->hasNext()) {

    if (print && (cq_nonpredicate.empty() || (**multi_iter).first->getColQualifierStr().find(cq_nonpredicate) != std::string::npos)) {
      std::cout << "has next " << (**multi_iter).first << std::endl;
      std::stringstream ss;
      ss << (**multi_iter).first;
      total_size += ss.str().size();
    }
    else if (size){
      total_size+=(**multi_iter).first->length();
      auto cf = (**multi_iter).first->getColFamilyStr();
      if (cf.find("fi") != std::string::npos){
        if (fi_counts.find(cf) != fi_counts.end()){
          fi_counts[cf]= fi_counts[cf]+1;
        }
        else{
          fi_counts[cf] = 1;
        }
      }
      else{
          if (!fi_counts.empty()){
            for(auto cfi: fi_counts){
              std::cout << cfi.first << " " << cfi.second << std::endl;
            }
            fi_counts.clear();
          }
      }
    }

    multi_iter->next();

    count++;
    if ((count % 100000) == 0)
      cntr.fetch_add(100000, std::memory_order_relaxed);
    
  }

if (!fi_counts.empty()){
            for(auto cfi: fi_counts){
              std::cout << cfi.first << " " << cfi.second << std::endl;
            }
            fi_counts.clear();
          }
  if (predicate){
    auto keySkipper = std::dynamic_pointer_cast<KeySkipper>(predicate);
    if (keySkipper){
      total_size += keySkipper->getSkippedLength();
    }
  }
  std::cout << "closing iterator" << std::endl;
  multi_iter->close();


  if (print || size)
  std::cout << "Bytes accessed " << total_size << std::endl;
  
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
  std::string visibility;
  bool print = false,size=false;
  long iterations = 1;
  int threads = 1;
  std::shared_ptr<cclient::data::KeyPredicate> predicate=nullptr;
  std::string row_contains="";
  std::string cq_nonpredicate="";

  if (argc >= 2) {
    for (int i = 1; i < argc; i++) {
      // always assume big endian
      std::string key = argv[i];
      if (key == "-p") {
        print = true;
      }

      if (key == "-t") {
         if (i + 1 < argc) {
          threads = std::atoi( argv[i + 1] );
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply thread count");
          
        }
      }

      if (key == "-s") {
        size = true;
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

      if (key == "-row") {
        if (i + 1 < argc) {
          std::string row_contains = argv[i + 1];
          predicate = std::make_shared<RowPredicate>(row_contains);
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply rfile");
        }
      }

      if (key == "-skip") {
        if (i + 1 < argc) {
          size_t skipCount= std::atol( argv[i + 1] );
          predicate = std::make_shared<KeySkipper>(skipCount);
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply rfile");
        }
      }

      if (key == "-cq") {
        if (i + 1 < argc) {
          std::string row_contains = argv[i + 1];
          predicate = std::make_shared<ColumnQualifierPredicate>(row_contains);
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply rfile");
        }
      }

      if (key == "-cqpost") {
        if (i + 1 < argc) {
          cq_nonpredicate = argv[i + 1];
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply rfile");
        }
      }

      if (key == "-i") {
        if (i + 1 < argc) {
          iterations = std::stoi(argv[i + 1]);
          i++;
        } else {
          throw std::runtime_error("Invalid number of arguments. Must supply nmumber of iterations");
        }
      }
    }
  }

  if (!rfiles.empty()) {
    for(long i=0; i < iterations; i++){
      readRfile(rfiles, 0, threads, print,size, visibility,predicate,cq_nonpredicate);
    }
  }

  return 0;
}

