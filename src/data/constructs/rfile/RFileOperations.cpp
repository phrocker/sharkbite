#include "data/constructs/rfile/RFileOperations.h"
#include "data/streaming/input/HdfsInputStream.h"
#include "data/streaming/HdfsOutputStream.h"
#include "data/streaming/OutputStream.h"

namespace cclient {
namespace data {

std::shared_ptr<cclient::data::SequentialRFile> RFileOperations::openForWrite(const std::string &rfile) {

  cclient::data::streams::OutputStream *stream;
  std::vector<cclient::data::streams::OutputStream*> ownedStreams;
  if (rfile.find("hdfs://") != std::string::npos) {
    stream = new cclient::data::streams::HdfsOutputStream(rfile);
    ownedStreams.push_back(stream);
  } else {
    auto in = std::make_unique<std::ofstream>(rfile, std::ifstream::ate | std::ifstream::binary);

    auto os = new cclient::data::streams::OutputStream(std::move(in), 0);

    stream = new cclient::data::streams::EndianTranslationStream(os);

    ownedStreams.push_back(stream);

    ownedStreams.push_back(os);

  }


  auto compressor = std::make_unique<cclient::data::compression::ZLibCompressor>(256 * 1024);

  auto bcFile = std::make_unique<cclient::data::BlockCompressedFile>(std::move(compressor));

  auto shrd = std::make_shared<cclient::data::SequentialRFile>(stream, std::move(bcFile));

  shrd->addStreams(ownedStreams);

  return shrd;
}

cclient::data::RFile* RFileOperations::open(const std::string &rfile) {

  auto ifs = std::make_unique<std::ifstream>(rfile.c_str(), std::ifstream::binary | std::ifstream::in);

  cclient::data::streams::InputStream *stream = new cclient::data::streams::InputStream(std::move(ifs), 0);

  stream = new cclient::data::streams::EndianInputStream(stream);

  std::fstream::pos_type size = filesize(rfile.c_str());

  return new cclient::data::RFile(stream, size);
}

std::shared_ptr<cclient::data::SequentialRFile> RFileOperations::openSequential(const std::string &rfile) {
  size_t size = 0;
  std::unique_ptr<cclient::data::streams::InputStream> stream;
  if (rfile.find("hdfs://") != std::string::npos) {
    auto str = std::make_unique<cclient::data::streams::HdfsInputStream>(rfile);
    size = str->getFileSize();
    stream = std::move(str);
  } else {
    size = filesize(rfile.c_str());
    auto in = std::make_unique<std::ifstream>(rfile, std::ifstream::ate | std::ifstream::binary);

    stream = std::make_unique<cclient::data::streams::InputStream>(std::move(in), 0);
  }

  auto endstream = std::make_unique<cclient::data::streams::ReadAheadInputStream>(std::move(stream), 128 * 1024, 1024 * 1024, size);

  return std::make_shared<cclient::data::SequentialRFile>(std::move(endstream), size);
}

std::shared_ptr<cclient::data::streams::KeyValueIterator> RFileOperations::openManySequential(const std::vector<std::string> &rfiles) {
  std::vector<std::shared_ptr<cclient::data::streams::KeyValueIterator>> iters;
  std::vector<std::future<std::shared_ptr<cclient::data::streams::KeyValueIterator>>> futures;
  for (const auto &path : rfiles) {
    futures.emplace_back( std::async([path](void) ->std::shared_ptr<cclient::data::streams::KeyValueIterator> {
        size_t size = 0;
        std::unique_ptr<cclient::data::streams::InputStream> stream;
        if (path.find("hdfs://") != std::string::npos) {
          auto str = std::make_unique<cclient::data::streams::HdfsInputStream>(path);
          size = str->getFileSize();
          stream = std::move(str);
        } else {
          size = filesize(path.c_str());
          auto in = std::make_unique<std::ifstream>(path, std::ifstream::ate | std::ifstream::binary);

          stream = std::make_unique<cclient::data::streams::InputStream>(std::move(in), 0);
        }

        auto endstream = std::make_unique<cclient::data::streams::ReadAheadInputStream>(std::move(stream), 128 * 1024, 1024 * 1024, size);

        return  std::make_shared<cclient::data::SequentialRFile>(std::move(endstream), size);
      
    }));
    
      
  }
  for(auto &future : futures){
      auto res = future.get();
      if (res)
        iters.push_back(res);
      else{
        throw std::runtime_error("Error while opening rfile");
      }
  }
  return std::make_shared<cclient::data::MultiIterator>(iters);
}

std::ifstream::pos_type RFileOperations::filesize(const char *filename) {
  std::ifstream in(filename, std::ifstream::ate | std::ifstream::binary);
  return in.tellg();
}

}
}
