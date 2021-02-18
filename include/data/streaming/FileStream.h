
#pragma once

#include <memory>

#include "data/streaming/input/InputStream.h"

namespace cclient {
namespace data {
namespace streams {

struct FileStream {
  std::unique_ptr<cclient::data::streams::InputStream> stream;
  size_t filesize;

  explicit FileStream(
      std::unique_ptr<cclient::data::streams::InputStream> stream,
      size_t filesize)
      : stream(std::move(stream)), filesize(filesize) {}

  FileStream(const FileStream &) = delete;
  FileStream(FileStream &&o) {
    stream = std::move(o.stream);
    filesize = std::move(o.filesize);
  }

  FileStream &operator=(FileStream &&o) {
    stream = std::move(o.stream);
    filesize = std::move(o.filesize);
    return *this;
  }
};

}  // namespace streams
}  // namespace data
}  // namespace cclient