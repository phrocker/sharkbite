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
#ifndef KEY
#define KEY 1

#include "../streaming/Streams.h"
#include "AllocatorPool.h"
#include "data/extern/fastmemcpy/FastMemCpy.h"
#include "Text.h"

#include <stdint.h>
#include <ostream>
#include <memory>
#include <stdio.h>
#include <string.h>

namespace cclient {
namespace data {

/**
 * Key
 */
class Key : public cclient::data::streams::StreamInterface, public std::enable_shared_from_this<Key> {

 public:

  explicit Key(cclient::data::ArrayAllocatorPool *pool);

  Key(const char *const userRow = nullptr);

  explicit Key(const std::string &row, const std::string &cf = "", const std::string &cq = "", const std::string &cv = "", int64_t ts = 9223372036854775807L)
      :
      Key() {
    setRow(row);
    setColFamily(cf);
    setColQualifier(cq);
    setColVisibility(cv);
    timestamp = ts;
    deleted = false;
  }

  explicit Key(const char *rw, const char *cf = nullptr, const char *cq = nullptr, const char *cv = nullptr, int64_t ts = 9223372036854775807L)
      :
      Key() {
    if (rw)
      setRow(rw, strlen(rw));
    if (cf)
      setColFamily(cf, strlen(cf));
    if (cq)
      setColQualifier(cq, strlen(cq));
    if (cv)
      setColVisibility(cv, strlen(cv));
    timestamp = ts;
    deleted = false;
  }

  /**
   * Constructor that deep copies another key. Does not subsume ownership.
   **/
  explicit Key(std::shared_ptr<Key> other)
      :
      Key() {
    std::pair<char*, size_t> row = other->getRow();

    setRow(row.first, row.second);

    std::pair<char*, size_t> cf = other->getColFamily();

    setColFamily(cf.first, cf.second);

    std::pair<char*, size_t> cq = other->getColQualifier();

    setColQualifier(cq.first, cq.second);

    std::pair<char*, size_t> cv = other->getColVisibility();

    setColVisibility(cv.first, cv.second);

    setTimeStamp(other->getTimeStamp());

    setDeleted(other->isDeleted());
  }

  virtual
  ~Key();

  bool empty() {
    return rowMaxSize == 0 && columnFamilySize == 0 && colQualSize == 0 && colVisSize == 0;
  }

  void setRow(const char *r, uint32_t size, bool takeOwnership = false) {
    setRow(r, size, size, takeOwnership);
  }
  void setRow(const char *r, uint32_t size, uint32_t maxSize, bool takeOwnership = false);

  // set via text objects, incrementing the reference counter for Text
  void setRow(Text*);
  void setColumnFamily(Text*);
  void setColumnQualifier(Text*);
  void setColumnVisibility(Text*);

  void setRow(const std::string &row) {
    setRow(row.c_str(), row.length());
  }

  std::pair<char*, size_t> getRow() {
    if (row_ref && !row_ref->empty())
      return row_ref->getBuffer();
    else
      return std::make_pair(row, rowLength);
  }

  std::string getRowStr() {
    if (row_ref && !row_ref->empty())
      return row_ref->toString();
    return std::string(row, rowLength);
  }

  void setColFamily(const char *r, uint32_t size, bool takeOwnership = false){
    setColFamily(r,size,size,takeOwnership);
  }


  void setColFamily(const char *r, uint32_t size,uint32_t maxSize, bool takeOwnership = false);

  void setColFamily(const std::string &st) {
    setColFamily(st.c_str(), st.size());
  }

  inline std::pair<char*, size_t> getColFamily() {
    if (cf_ref && !cf_ref->empty())
      return cf_ref->getBuffer();
    return std::make_pair(colFamily, columnFamilyLength);
  }

  inline std::string getColFamilyStr() {
    if (cf_ref && !cf_ref->empty())
      return cf_ref->toString();
    return std::string(colFamily, columnFamilyLength);
  }

  void setColQualifier(const char *r, uint32_t size, bool takeOwnership = false){
    setColQualifier(r,size,size,takeOwnership);
  }

  void setColQualifier(const char *r, uint32_t size, uint32_t maxSize , bool takeOwnership = false);


  void setColQualifier(const std::string &st) {
    setColQualifier(st.c_str(), st.size(), st.size(),false);
  }

  std::pair<char*, size_t> getColQualifier() {
    if (cq_ref && !cq_ref->empty())
      return cq_ref->getBuffer();
    return std::make_pair(colQualifier, colQualLen);
  }

  std::string getColQualifierStr() {
    if (cq_ref && !cq_ref->empty())
      return cq_ref->toString();
    return std::string(colQualifier, colQualLen);
  }

  void setColVisibility(const char *r, uint32_t size, bool takeOwnership = false){
    setColVisibility(r,size,size,takeOwnership);
  }


  void setColVisibility(const char *r, uint32_t size,uint32_t maxSize, bool takeOwnership = false);


  void setColVisibility(const std::string &st) {
    setColVisibility(st.c_str(), st.size());
  }

  std::pair<char*, size_t> getColVisibility() {
    if (cv_ref && cv_ref->empty())
      return cv_ref->getBuffer();
    return std::make_pair(keyVisibility, colVisSize);
  }

  std::string getColVisibilityStr() {
    if (cv_ref && cv_ref->empty())
      return cv_ref->toString();
    return std::string(keyVisibility, colVisSize);
  }

  int64_t getTimeStamp() {
    return timestamp;
  }

  void setTimeStamp(const int64_t ts) {
    timestamp = ts;
  }

  bool isDeleted() {
    return deleted;
  }

  void setDeleted(bool isDeleted) {
    deleted = isDeleted;
  }

  std::shared_ptr<StreamInterface> getStream() {
    return shared_from_this();
  }

  bool operator <=(const Key *rhs) const {
    if (rhs == nullptr)
      return false;
    return *this < *rhs || *rhs == *this;
  }

  bool operator <=(const Key &rhs) const {
    if (rhs == nullptr)
      return false;
    return *this < rhs || rhs == *this;
  }

  bool
  operator <(const Key &rhs) const;

  bool operator <(const Key *rhs) const {
    if (rhs == nullptr)
      return false;
    return *this < *rhs;
  }

  bool
  operator ==(const Key &rhs) const;
  bool operator !=(const Key &rhs) const {
    return !(*this == rhs);
  }

  bool operator ==(const Key *rhs) const {
    if (rhs == nullptr)
      return false;
    return *this == *rhs;
  }
  bool operator !=(const Key *rhs) const {
    if (rhs == nullptr)
      return true;
    return !(*this == *rhs);
  }

  std::string toString() {
    std::string out = "";
    std::pair<char*, size_t> row = getRow();
    out += std::string(row.first, row.second) + " ";
    std::pair<char*, size_t> cf = getColFamily();
    std::pair<char*, size_t> cq = getColQualifier();
    out += std::string(cf.first, cf.second) + ":" + std::string(cq.first, cq.second) + " [";
    std::pair<char*, size_t> viz = getColVisibility();
    auto vizstring = viz.second > 1 ? std::string(viz.first, viz.second) : "";
    out += vizstring + "] " + std::to_string(getTimeStamp());
    return out;
  }

  friend inline std::ostream&
  operator <<(std::ostream &out, Key &rhs) {
    std::pair<char*, size_t> row = rhs.getRow();
    out << std::string(row.first, row.second) << " ";
    std::pair<char*, size_t> cf = rhs.getColFamily();
    std::pair<char*, size_t> cq = rhs.getColQualifier();
    out << (cf.second > 0 ? std::string(cf.first, cf.second) : "") << ":" << (cq.second > 0 ? std::string(cq.first, cq.second) : "") << " [";
    std::pair<char*, size_t> viz = rhs.getColVisibility();
    auto vizstring = viz.second > 1 ? std::string(viz.first, viz.second) : "";
    out << vizstring << "] " << std::to_string(rhs.getTimeStamp());
    return out;
  }

  friend inline std::ostream&
  operator <<(std::ostream &out, Key *rhs) {
    return operator<<(out, *rhs);
  }

  uint64_t
  write(cclient::data::streams::OutputStream *outStream);

  uint64_t
  read(cclient::data::streams::InputStream *in);
 protected:

  inline void reclaim(char**, size_t, Text**);

  /**
   * Row part of key
   */
  char *row;
  uint32_t rowMaxSize;
  uint32_t rowLength;

  /**
   * Column family
   */
  uint32_t columnFamilyLength;
  char *colFamily;
  uint32_t columnFamilySize;

  /**
   * Column qualifier.
   */
  char *colQualifier;
  uint32_t colQualSize;
  uint32_t colQualLen;
  char *keyVisibility;
  uint32_t colVisSize;
  uint32_t colVisLen;
  int64_t timestamp;
  bool deleted;

  cclient::data::ArrayAllocatorPool *objectPool;
  Text *row_ref;
  Text *cf_ref;
  Text *cq_ref;
  Text *cv_ref;

  /**
   * copied from writable comparable utils
   */
  static int compareBytes(const char *b1, int s1, int l1, const char *b2, int s2, int l2) {
    int end1 = s1 + l1;
    int end2 = s2 + l2;
    for (int i = s1, j = s2; i < end1 && j < end2; i++, j++) {
      int a = (b1[i] & 0xff);
      int b = (b2[j] & 0xff);
      if (a != b) {
        return a - b;
      }
    }
    return l1 - l2;
  }
};

} /* namespace data */
}/* namespace cclient */

#endif

