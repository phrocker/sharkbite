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

#include "data/constructs/rfile/bcfile/meta_index.h"

namespace cclient {
namespace data {

MetaIndex::MetaIndex() {

}

MetaIndex::~MetaIndex() {

}

/**
 * Prepares a new MetaIndexEntry for this meta index table.
 * @param name name of index entry
 * @param comp compressor. not constant as this will eventually
 * be used for modifications and compression ( thus potentially
 * modifying the internals of the compressor )
 * @returns newly allocated MetaIndexEntry
 */
MetaIndexEntry* MetaIndex::prepareNewEntry(const std::string name, std::unique_ptr<cclient::data::compression::Compressor> comp) {
  std::shared_ptr<MetaIndexEntry> entry = std::make_shared<MetaIndexEntry>(std::move(comp));
  entry->setName(name);
  addEntry(entry, NULL);
  return getEntry(name);

}

uint64_t MetaIndex::read(cclient::data::streams::InputStream *in) {
  uint64_t count = in->readHadoopLong();

  for (uint64_t i = 0; i < count; i++) {
    std::shared_ptr<MetaIndexEntry> entry = std::make_shared<MetaIndexEntry>(in);

    index.insert(std::make_pair(entry->getMetaName(), entry));
  }

  return in->getPos();
}

/**
 * @Override
 */
uint64_t MetaIndex::write(cclient::data::streams::OutputStream *out) {
  out->writeEncodedLong(index.size());
  // write out all the meta index entries
  for (std::map<std::string, std::shared_ptr<MetaIndexEntry>>::iterator it = index.begin(); it != index.end(); it++) {

    (*it).second->write(out);
  }
  return out->getPos();
}

std::map<std::string, std::shared_ptr<MetaIndexEntry>>* MetaIndex::getEntries() {
  return &index;
}

/**
 * Adds an entry to the Meta Index
 * @param indexEntry the Meta Index entry
 * @param out output stream, from which we gather the region
 * offset
 */
void MetaIndex::addEntry(const std::shared_ptr<MetaIndexEntry> &indexEntry, cclient::data::streams::DataOutputStream *out) {
  index[indexEntry->getMetaName()] = indexEntry;
  BlockRegion *region = index[indexEntry->getMetaName()]->getRegion();
  if (out != NULL) {
    region->setOffset(out->getPos());
  }
}

/**
 * Returns the entry associatd with the given name.
 * @param name name of entry we wish to retrieve
 * @returns MetaIndexEntry pointer. Not constant as we may
 * wish, and are allowed, to modify the index Entry. Note
 * that this may return null.
 */
MetaIndexEntry* MetaIndex::getEntry(const std::string &name) {
  return index[name].get();
}

}
}
