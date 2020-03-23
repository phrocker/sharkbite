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

#include "data/constructs/Key.h"

namespace cclient {
namespace data {

Key::Key(cclient::data::ArrayAllocatorPool *pool)
    :
    Key() {
  objectPool = pool;
}

Key::Key(const char *const userRow)
    :
    deleted(false),
    timestamp(9223372036854775807L),
    colVisSize(0),
    rowMaxSize(0),
    columnFamilySize(0),
    colQualSize(0),
    rowLength(0),
    columnFamilyLength(0),
    colQualLen(0),
    colVisLen(0),
    objectPool(nullptr),
    row_ref(nullptr),
    cf_ref(nullptr),
    cq_ref(nullptr),
    cv_ref(nullptr) {
  if (userRow)
    setRow(userRow, strlen(userRow));
  else {
    row = nullptr;

    colFamily = nullptr;

    colQualifier = nullptr;

    keyVisibility = nullptr;
  }

}

Key::~Key() {
  reclaim(&row, rowMaxSize, &row_ref);
  reclaim(&colFamily, columnFamilySize, &cf_ref);
  reclaim(&colQualifier, colQualSize, &cq_ref);
  reclaim(&keyVisibility, colVisSize, &cv_ref);
}

void Key::reclaim(char **val, size_t size, Text **textPtr) {
  if (*val) {
    if (objectPool) {
      objectPool->free(std::make_pair(*val, size));
      *val = nullptr;
    } else {
      delete[] *val;
      *val = nullptr;
    }
  }
  *textPtr = nullptr;
}

void Key::setRow(Text *rowRef) {
  if (row) {
    reclaim(&row, rowMaxSize, &row_ref);
  }
  row_ref = rowRef;
}

void Key::setRow(const char *r, uint32_t size, uint32_t maxsize, bool takeOwnership) {
  if (!takeOwnership) {
    if (maxsize > rowMaxSize) {
      delete[] row;
      row = new char[maxsize];
      rowMaxSize = maxsize;
    }

    memcpy_fast(row, r, size);
  } else {
    reclaim(&row, rowMaxSize, &row_ref);
    row = (char*) r;
    rowMaxSize = maxsize;
  }

  rowLength = size;

}

void Key::setColFamily(const char *r, uint32_t size, uint32_t maxsize, bool takeOwnership) {
  if (!takeOwnership) {
    if (maxsize > columnFamilySize) {
      delete[] colFamily;
      colFamily = new char[maxsize];
      columnFamilySize = maxsize;
    }

    memcpy_fast(colFamily, r, size);
  } else {
    reclaim(&colFamily, columnFamilySize, &cf_ref);
    colFamily = (char*) r;
    columnFamilySize = maxsize;
  }
  columnFamilyLength = size;

}

void Key::setColumnFamily(Text *col) {
  if (colFamily) {
    reclaim(&colFamily, columnFamilySize, &cf_ref);
  }
  cf_ref = col;
}

void Key::setColQualifier(const char *r, uint32_t size, uint32_t maxsize, bool takeOwnership) {

  if (!takeOwnership) {
    if (maxsize > colQualSize) {
      char *nr = new char[maxsize + 1];
      delete[] colQualifier;
      colQualifier = nr;
      colQualSize = maxsize;

    }

    memcpy_fast(colQualifier, r, size);
  } else {
    reclaim(&colQualifier, colQualSize, &cq_ref);
    colQualifier = (char*) r;
    colQualSize = maxsize;
  }
  colQualLen = size;

}

void Key::setColumnQualifier(Text *cq) {
  if (colQualifier) {
    reclaim(&colQualifier, colQualSize, &cq_ref);
  }
  cq_ref = cq;
}

void Key::setColVisibility(const char *r, uint32_t size, uint32_t maxsize, bool takeOwnership) {

  if (!takeOwnership) {
    if (maxsize > colVisSize) {
      delete[] keyVisibility;
      keyVisibility = new char[maxsize+1];
      colVisSize = maxsize;
    }
    memcpy_fast(keyVisibility, r, size);
  } else {
    reclaim(&keyVisibility, colVisSize, &cv_ref);
    keyVisibility = (char*) r;
    colVisSize = maxsize;
  }

  colVisLen = size;

}

void Key::setColumnVisibility(Text *cv) {
  if (keyVisibility) {
    reclaim(&keyVisibility, colVisSize, &cv_ref);
  }
  cv_ref = cv;
}

bool Key::operator <(const Key &rhs) const {
  int compare = compareBytes(row, 0, rowLength, rhs.row, 0, rhs.rowLength);

  if (compare < 0)
    return true;
  else if (compare > 0)
    return false;
  compare = compareBytes(colFamily, 0, columnFamilyLength, rhs.colFamily, 0, rhs.columnFamilyLength);

  if (compare < 0)
    return true;
  else if (compare > 0)
    return false;
  compare = compareBytes(colQualifier, 0, colQualLen, rhs.colQualifier, 0, rhs.colQualLen);

  if (compare < 0)
    return true;

  return false;
}

bool Key::operator ==(const Key &rhs) const {
  int compare = compareBytes(row, 0, rowLength, rhs.row, 0, rhs.columnFamilyLength);

  if (compare != 0)
    return false;

  compare = compareBytes(colFamily, 0, columnFamilyLength, rhs.colFamily, 0, rhs.columnFamilyLength);

  if (compare != 0)
    return false;

  compare = compareBytes(colQualifier, 0, colQualLen, rhs.colQualifier, 0, rhs.colQualLen);

  if (compare != 0)
    return false;

  return (timestamp < rhs.timestamp);

}

uint64_t Key::write(cclient::data::streams::OutputStream *outStream) {

  uint32_t offset = rowLength;

  outStream->writeHadoopLong(offset);
  //outStream->writeHadoopLong( offset ); // cf offset

  offset += columnFamilyLength;
  outStream->writeHadoopLong(offset);
  //outStream->writeHadoopLong( offset ); // cq offset

  offset += colQualLen;
  outStream->writeHadoopLong(offset);
  //outStream->writeHadoopLong( offset ); // colvis offset

  offset += colVisLen;
  outStream->writeHadoopLong(offset);
  //outStream->writeHadoopLong( offset ); // total

  outStream->writeBytes(row, rowLength);
  outStream->writeBytes(colFamily, columnFamilyLength);
  outStream->writeBytes(colQualifier, colQualLen);
  outStream->writeBytes(keyVisibility, colVisLen);
  outStream->writeHadoopLong(timestamp);
  //outStream->writeHadoopLong( timestamp);

  return outStream->writeBoolean(deleted);

}

uint64_t Key::read(cclient::data::streams::InputStream *in) {
  int colFamilyOffset = in->readEncodedLong();
  int colQualifierOffset = in->readEncodedLong();
  int colVisibilityOffset = in->readEncodedLong();
  int totalLen = in->readEncodedLong();

  row = new char[colFamilyOffset];
  in->readBytes(row, colFamilyOffset);
  rowLength = rowMaxSize = colFamilyOffset;

  colFamily = new char[colQualifierOffset - colFamilyOffset];
  in->readBytes(colFamily, colQualifierOffset - colFamilyOffset);
  columnFamilyLength = columnFamilySize = colQualifierOffset - colFamilyOffset;

  colQualifier = new char[colVisibilityOffset - colQualifierOffset];
  in->readBytes(colQualifier, colVisibilityOffset - colQualifierOffset);
  colQualLen = colQualSize = colVisibilityOffset - colQualifierOffset;

  keyVisibility = new char[totalLen - colVisibilityOffset];
  in->readBytes(keyVisibility, totalLen - colVisibilityOffset);
  colVisLen = colVisSize = totalLen - colVisibilityOffset;

  timestamp = in->readEncodedLong();

  deleted = in->readBoolean();

  return in->getPos();
}

} /* namespace data */
} /* namespace cclient */
