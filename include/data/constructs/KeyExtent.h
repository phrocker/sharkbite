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
#ifndef KEYEXTENT_H_
#define KEYEXTENT_H_

#include <iostream>
#include <memory>
#include "inputvalidation.h"
#include "../exceptions/ClientException.h"
#include "../exceptions/IllegalArgumentException.h"
#include "StructureDefinitions.h"
#include <string>

#include "value.h"

namespace cclient {
namespace data {

/**
 * Key Extent representation
 **/
class KeyExtent {
 public:
  /**
   * Constructor
   * @param flattenedText flattened representation of the end row and table id
   * @param prevEndRow the previous endrow so we can ensure lexicographical ordering
   **/
  KeyExtent(std::string flattenedText, std::shared_ptr<Value> prevEndRow);

  /**
   * Constructor
   * @param tableIdIn table Id
   * @param endRowIn end row for the tablet
   **/
  KeyExtent(std::string tableIdIn, std::string endRowIn)
      : KeyExtent(tableIdIn, endRowIn, "") {

  }

  /**
   Constructor that copies a key KeyExtent
   @param copy key extent to copy
   **/
  explicit KeyExtent(KeyExtent *copy)
      : tableId(copy->getTableId()),
        endRow(copy->getEndRow()),
        prevEndRow(copy->getPrevEndRow()) {
  }

  /**
   Constructor that manually sets all values
   @param tableIdIn table Id
   @param endRowIn end row
   @param prevEndRowIn previous end row
   **/
  KeyExtent(std::string tableIdIn, std::string endRowIn, std::string prevEndRowIn) {
    if (IsEmpty(&tableIdIn)) {
      throw cclient::exceptions::IllegalArgumentException("Table Id cannot be null");
    }
    if (!IsEmpty(&endRowIn) && !IsEmpty(&prevEndRowIn)) {
      if (prevEndRowIn.compare(endRowIn) >= 0) {
        throw cclient::exceptions::IllegalArgumentException("PrevEndRow >= endRow");
      }
    }
    setTableId(tableIdIn);
    endRow = endRowIn;
    prevEndRow = prevEndRowIn;

  }

  /**
   * Creates metadata entry using table and row
   * @param table
   * @param
   * @returns metadata entry row
   **/
  static std::string createMetadataEntry(std::string table, std::string row) {
    std::string entry(table);
    if (IsEmpty(&row)) {
      entry.append("<");
    } else {
      entry.append(";");
      entry.append(row);
    }

    return entry;
  }

  ~KeyExtent();

  KeyExtent&
  operator=(const KeyExtent& other) {
    tableId = other.tableId;
    endRow = other.endRow;
    prevEndRow = other.prevEndRow;
    return *this;
  }

  bool operator <(const KeyExtent &rhs) const {
    int result = tableId.compare(rhs.tableId);

    if (result < 0)
      return result;
    else if (result > 0)
      return false;

    result = endRow.compare(rhs.endRow);

    if (result < 0)
      return result;
    else if (result > 0)
      return false;

    result = prevEndRow.compare(rhs.prevEndRow);

    if (result < 0)
      return true;
    else
      return false;
  }

  bool operator ==(const KeyExtent &rhs) const {
    int result = tableId.compare(rhs.tableId);

    if (result != 0)
      return (result == 0);

    result = endRow.compare(rhs.endRow);

    if (result != 0)
      return (result == 0);

    result = prevEndRow.compare(rhs.prevEndRow);

    if (result != 0)
      return (result == 0);

    return true;
  }

  friend inline std::ostream &
  operator <<(std::ostream &out, KeyExtent *rhs) {

    out << "tableId:" << rhs->tableId << " end:" << rhs->endRow << " prev:" << rhs->prevEndRow << " " << std::endl;
    return out;
  }
  void setTableId(std::string id) {
    tableId = id;
  }

  const std::string getTableId() const {
    return tableId;
  }

  const std::string getEndRow() const {
    return endRow;
  }

  std::string getPrevEndRow() const {
    return prevEndRow;
  }

  void setPrevEndRow(std::string prev) {
    prevEndRow = prev;
  }
 protected:
  void setPrevEndRow(std::shared_ptr<Value> prevEndRow) {
    std::pair<unsigned char *, size_t> valuePair = prevEndRow->getValue();
    setPrevEndRow((char*) valuePair.first, valuePair.second);

  }
  void setPrevEndRow(char *text, size_t text_len) {
    prevEndRow = std::string(text + 1, text_len - 1);
  }

  /**
   * Decodes the metadata row . Usually in the form of tableId;row
   * or tableId<;
   * @param flattenedText text object that must be 'unflattened'
   **/
  void decodeMetadataRow(std::string flattenedText) {
    int16_t semiPos = -1;
    int16_t ltPos = -1;

    if (flattenedText.at(flattenedText.size() - 1) == '<') {
      ltPos = flattenedText.size() - 1;
    } else {
      for (uint32_t i = 0; i < flattenedText.size(); i++) {
        if (flattenedText.at(i) == ';') {
          semiPos = i;
          break;
        }
      }
    }
    if (semiPos < 0 && ltPos < 0) {
      throw new cclient::exceptions::ClientException("Metadata row does not contain ; or <");
    }

    if (semiPos < 0) {
      tableId = flattenedText.substr(0, flattenedText.size() - 1);
      endRow = "";
    } else {

      tableId = flattenedText.substr(0, semiPos);

      endRow = flattenedText.substr(semiPos + 1, flattenedText.size() - (semiPos + 1));

    }

  }
  // table id
  std::string tableId;
  // end for for this key extent.
  std::string endRow;
  // previous end row.
  std::string prevEndRow;
};
// static variable for the root tablet key extent.
static const KeyExtent ROOT_TABLET_EXTENT(
METADATA_TABLE_ID,
KeyExtent::createMetadataEntry (METADATA_TABLE_ID, ""));

}
/* namespace data */
} /* namespace cclient */
#endif /* KEYEXTENT_H_ */
