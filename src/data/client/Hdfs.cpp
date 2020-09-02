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
#include "data/constructs/client/Hdfs.h"

#include "data/streaming/HdfsOutputStream.h"
#include "data/streaming/input/HdfsInputStream.h"
#include "utils/Uri.h"

namespace cclient {
namespace data {
namespace hdfs {

HdfsDirEnt::HdfsDirEnt(const hdfsFileInfo *entry)
    : name(entry->mName),
      size(entry->mSize),
      repFactor(entry->mReplication),
      owner(entry->mOwner),
      group(entry->mGroup),
      isDir(entry->mKind == kObjectKindDirectory) {}

HdfsLink::HdfsLink(std::string nn) : nn(nn) {
  auto uri = utils::Uri(nn);
  reference = hdfsConnect(this->nn.c_str(), uri.port());
}

HdfsLink::HdfsLink(std::string nn, int port) : nn(nn), port(port) {
  reference = hdfsConnect(this->nn.c_str(), port);
}

int HdfsLink::mkdir(const std::string &dir) {
  return hdfsCreateDirectory(reference, dir.c_str());
}

int HdfsLink::remove(const std::string &dir, bool recursive) {
  return hdfsDelete(reference, dir.c_str(), recursive);
}

int HdfsLink::rename(const std::string &fromName, const std::string toName) {
  return hdfsRename(reference, fromName.c_str(), toName.c_str());
}

int HdfsLink::move(const std::string &from_path, const std::string &to_path) {
  return hdfsRename(reference, from_path.c_str(), to_path.c_str());
}

int HdfsLink::chmod(const std::string &dir, int perm) {
  return hdfsChmod(reference, dir.c_str(), perm);
}

int HdfsLink::chown(const std::string &dir, const std::string &user,
                    const std::string &group) {
  return hdfsChown(reference, dir.c_str(), user.c_str(), group.c_str());
}

std::shared_ptr<cclient::data::streams::ByteOutputStream> HdfsLink::write(
    const std::string &path) {
  return std::make_shared<cclient::data::streams::HdfsOutputStream>(
      shared_from_this(), path);
}

std::shared_ptr<cclient::data::streams::InputStream> HdfsLink::read(
    const std::string &path) {
  return std::make_shared<cclient::data::streams::HdfsInputStream>(
      shared_from_this(), path);
}

std::vector<HdfsDirEnt> HdfsLink::list(const std::string &dir) {
  std::vector<HdfsDirEnt> entries;
  int entryCount = 0;
  auto ptr = hdfsListDirectory(reference, dir.c_str(), &entryCount);

  for (int i = 0; i < entryCount; i++) {
    auto entry = ptr[i];
    entries.emplace_back(HdfsDirEnt(&entry));
  }

  hdfsFreeFileInfo(ptr, entryCount);
  return entries;
}

HdfsLink::~HdfsLink() { hdfsDisconnect(reference); }

hdfsFS HdfsLink::getHdfsReference() { return reference; }

}  // namespace hdfs
}  // namespace data
}  // namespace cclient
