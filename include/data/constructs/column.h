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

#ifndef COLUMN_H
#define COLUMN_H

#include <stdint.h>

#include <stdio.h>
#include <string.h>

#include "../streaming/Streams.h"

namespace cclient {
namespace data {

/**
 * Column representation
 **/
class Column : public cclient::data::streams::StreamInterface {

 public:

  /**
   * Constructor
   * @param columFam column family.
   **/
  explicit Column(const std::string &columFam)
      :
      Column(columFam, std::string(), std::string()) {

  }

  /**
   * Constructor
   * @param columFam column family.
   * @param columnQual column qualifier
   **/
  explicit Column(const std::string &columFam, const std::string &columnQual)
      :
      Column(columFam, columnQual, std::string()) {

  }

  /**
   * Constructor
   * @param columFam column family.
   * @param columnQual column qualifier
   * @param columnVis column visibility
   **/
  explicit Column(const std::string &columnFam, const std::string &columnQual, const std::string &columnVis)
      :
      columnFamily(columnFam),
      columnQualifier(columnQual),
      columnVisibility(columnVis) {

  }

  /**
   * Destructor
   **/
  virtual ~Column();
  virtual bool operator==(const Column &other) const;
  bool operator <(const Column &rhs) const;

  bool operator <(const Column *rhs) const {
    return *this < *rhs;
  }
  uint64_t write(cclient::data::streams::OutputStream *outStream);

  void setColFamily(const char *r, uint32_t size);

  std::string getColFamily() const {
    return columnFamily;
  }

  void setColQualifier(const char *r, uint32_t size);

  std::string getColQualifier() const {
    return columnQualifier;
  }

  void setColVisibility(const char *r, uint32_t size);

  std::string getColVisibility() const {
    return columnVisibility;
  }

 private:

  std::string columnFamily;
  std::string columnQualifier;
  std::string columnVisibility;
  /*
   char *columnFamily;
   uint16_t columnFamilyLen;

   char *columnQualifier;
   uint16_t columnQualifierLen;

   char *columnVisibility;
   uint16_t columnVisibilityLen;
   */
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
}
}
#endif // COLUMN_H
