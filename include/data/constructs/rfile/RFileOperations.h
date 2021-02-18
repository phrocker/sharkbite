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

#include "data/streaming/accumulo/KeyValueIterator.h"
#include "data/constructs/predicates/key/KeyPredicates.h"
#include "data/streaming/input/ReadAheadInputStream.h"

#include "data/constructs/ageoff/AgeOffConditions.h"
#include "data/iterators/MultiIterator.h"
#include "data/iterators/DeletingMultiIterator.h"
#include "data/iterators/VersioningIterator.h"
#include "data/streaming/HdfsOutputStream.h"
#include "data/streaming/OutputStream.h"
#include "data/streaming/input/HdfsInputStream.h"
#include "data/constructs/rfile/ParallelRFile.h"
namespace cclient{

namespace data{

class RFileOperations{
public:
   static std::ifstream::pos_type filesize(const char* filename);
   static std::shared_ptr<cclient::data::SequentialRFile> openForWrite(const std::string &path);
   static std::shared_ptr<cclient::data::SequentialRFile> write(const std::string &path, uint32_t compress_block_size = 32*1024);
   static cclient::data::RFile *open(const std::string &);
   static std::shared_ptr<cclient::data::SequentialRFile> openSequential(const std::string &);
  
  //template<typename T>
///   std::shared_ptr<cclient::data::streams::KeyValueIterator> openRFiles(const std::vector<std::string>  &rfiles,const std::shared_ptr<cclient::data::KeyPredicate> &predicate,int versions=0, bool withDeletes = false, bool propogate = true, uint64_t maxtimestamp=0);
  /**
   * Returns a merged read across the rfiles
   * @param rfiles absolute path to RFiles in HDFS or on the local file system
   * @return shared pointer to a multi iterator read
   */
   static std::shared_ptr<cclient::data::streams::KeyValueIterator> openManySequential(const std::vector<std::string>  &rfiles,int versions=0, bool withDeletes = false, bool propogate = true, uint64_t maxtimestamp=0);


  /**
   * Returns a merged read across the rfiles
   * @param rfiles absolute path to RFiles in HDFS or on the local file system
   * @return shared pointer to a multi iterator read
   */
   static std::shared_ptr<cclient::data::streams::KeyValueIterator> openManySequentialWithPredicate(const std::vector<std::string>  &rfiles,const std::shared_ptr<cclient::data::KeyPredicate> &predicate, int versions=0, bool withDeletes = false, bool propogate = true, uint64_t maxtimestamp=0);


static std::shared_ptr<cclient::data::streams::KeyValueIterator> openParallelRFiles(const std::vector<std::string>  &rfiles,const std::shared_ptr<cclient::data::KeyPredicate> &predicate,int versions=0, bool withDeletes=false, bool propogate=true, uint64_t maxtimestamp=0);
  

template<typename T>
 static std::shared_ptr<cclient::data::streams::KeyValueIterator> openRFiles(const std::vector<std::string>  &rfiles,const std::shared_ptr<cclient::data::KeyPredicate> &predicate,int versions, bool withDeletes, bool propogate, uint64_t maxtimestamp)
{
   std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>> iters;
  std::vector<
      std::future<std::shared_ptr<cclient::data::streams::KeyValueIterator>>>
      futures;
  std::shared_ptr<cclient::data::AgeOffEvaluator> ageOffEval = nullptr;
  if (maxtimestamp > 0) {
    ageOffEval = std::make_shared<cclient::data::AgeOffEvaluator>(
        cclient::data::AgeOffCondition(AgeOffType::DEFAULT, "default",
                                       maxtimestamp));
  }
  if (rfiles.size() > 1) {
    for (const auto &path : rfiles) {
      futures.emplace_back(std::async(
          [path, ageOffEval,predicate](void)
              -> std::shared_ptr<cclient::data::streams::KeyValueIterator> {
            try {
              size_t size = 0;
              std::unique_ptr<cclient::data::streams::InputStream> stream;
              if (path.find("hdfs://") != std::string::npos) {
                auto str =
                    std::make_unique<cclient::data::streams::HdfsInputStream>(
                        path);
                size = str->getFileSize();
                stream = std::move(str);
              } else {
                size = RFileOperations::filesize(path.c_str());
                auto in = std::make_unique<std::ifstream>(
                    path, std::ifstream::ate | std::ifstream::binary);

                stream = std::make_unique<cclient::data::streams::InputStream>(
                    std::move(in), 0);
              }

              auto endstream = std::make_unique<
                  cclient::data::streams::ReadAheadInputStream>(
                  std::move(stream), 128 * 1024, 1024 * 1024, size);
              auto rfstream = std::make_shared<T>(
                  std::move(endstream), size);
              if (predicate){
                rfstream->setKeyPredicate(predicate);
              }
              rfstream->setAgeOff(ageOffEval);
              return rfstream;
            } catch (const std::exception &e) {
              std::cout << e.what() << std::endl;
            }
            return nullptr;
          }));
    }
    for (auto &future : futures) {
      auto res = future.get();
      if (res)
        iters.push_back(res);
      else {
        throw std::runtime_error("Error while opening rfile");
      }
    }
  } else if (rfiles.size() == 1) {
    size_t size = 0;
    std::string path = rfiles.at(0);
    std::unique_ptr<cclient::data::streams::InputStream> stream;
    if (path.find("hdfs://") != std::string::npos) {
      auto str =
          std::make_unique<cclient::data::streams::HdfsInputStream>(path);
      size = str->getFileSize();
      stream = std::move(str);
    } else {
      size = RFileOperations::filesize(path.c_str());
      auto in = std::make_unique<std::ifstream>(
          path, std::ifstream::ate | std::ifstream::binary);

      stream = std::make_unique<cclient::data::streams::InputStream>(
          std::move(in), 0);
    }

    auto endstream =
        std::make_unique<cclient::data::streams::ReadAheadInputStream>(
            std::move(stream), 48 * 1024, 64 * 1024, size);

    auto rfstream = std::make_shared<T>(
        std::move(endstream), size);
    rfstream->setAgeOff(ageOffEval);
    if (predicate){
        rfstream->setKeyPredicate(predicate);
    }
    iters.emplace_back(rfstream);
  }

  std::shared_ptr<cclient::data::HeapIterator> heapItr;

  if (versions == 0) {
    if (!withDeletes) {
      heapItr = std::make_shared<cclient::data::MultiIterator>(iters);
    } else {
      heapItr = std::make_shared<cclient::data::DeletingMultiIterator>(
          iters, propogate);
    }
  } else {
    heapItr = std::make_shared<cclient::data::VersioningIterator>(iters);
  }

  return heapItr;
  }
};


}


}



#endif
