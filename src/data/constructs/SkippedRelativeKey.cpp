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
#include "data/constructs/SkippedRelativeKey.h"

namespace cclient {
namespace data {

bool SkippedRelativeKey::readPrefix(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame,int fieldsPrefixed, TextBuffer *field, TextBuffer *prevField, TextBuffer *stopField) {
  if ((fieldsSame & SAME_FIELD) != SAME_FIELD) {
    //auto tmp = field;
    TextBuffer tmp;
    tmp.buffer = field->buffer;
    tmp.size =field->size;
    tmp.max = field->max;

    field->buffer = prevField->buffer;
    field->size = prevField->size;
    field->max = prevField->max;

  
    prevField->buffer = tmp.buffer;
    prevField->size = tmp.size;
    prevField->max = tmp.max;

  
    
    
    if ((fieldsPrefixed & PREFIX) == PREFIX) {
      readPrefix(stream, field, prevField);
    } else
      read(stream, field);

    if (*field >= *stopField) {
      if (*field > *stopField) {
        *comparison = 1;
      } else
        *comparison = 1;
    } else {
      *comparison = -1;
    }
    return true;
  }
  return false;
}

void SkippedRelativeKey::readValue(cclient::data::streams::InputStream *stream, std::vector<char>  *val) {
  uint32_t len = stream->readInt();
  char *array = new char[len];
    stream->readBytes(array, len);

    val->insert(val->begin(), array, array + len);
    delete[] array;
}

void SkippedRelativeKey::read(cclient::data::streams::InputStream *stream,  TextBuffer *row) {
  uint32_t len = stream->readEncodedLong();
  read(stream, row, len);
}

void SkippedRelativeKey::read(cclient::data::streams::InputStream *stream, TextBuffer *row, uint32_t len) {
  
  auto bfr = allocatorInstance->allocateBuffer(len);
  row->buffer = bfr.first;
  row->max = bfr.second;

  row->size = len;
  stream->readBytes(row->buffer, len);
}

void SkippedRelativeKey::readPrefix(cclient::data::streams::InputStream *stream,  TextBuffer *row, TextBuffer *prevRow) {
 uint32_t prefixLen = stream->readHadoopLong();
  uint32_t remainingLen = stream->readHadoopLong();

  auto bfr = allocatorInstance->allocateBuffer(prefixLen + remainingLen + 1);

  row->buffer = bfr.first;
  row->max = bfr.second;
  memcpy_fast(row->buffer, prevRow->buffer, prefixLen);
  row->size = prefixLen + remainingLen;

  stream->readBytes(row->buffer + prefixLen, remainingLen);
}

bool SkippedRelativeKey::fastSkip(cclient::data::streams::InputStream *stream, const std::shared_ptr<Key> &seekKey, std::vector<char> *valCopy, std::shared_ptr<Key> prevKey,
                                  std::shared_ptr<Key> currKey, size_t entriesRemaining) {

  TextBuffer row, cf, cq, cv;
  TextBuffer prevRow, prevCf, prevCq, prevVis;

  //TextBuffer scratch = seekKey->getRow();
  TextBuffer stopRow, stopCf, stopCq, stopCv;
  stopRow = seekKey->getRow();
  //stopRow.insert(stopRow.end(), scratch.first, scratch.first + scratch.second);
  stopCf = seekKey->getColFamily();
  //stopCf.insert(stopCf.end(), scratch.first, scratch.first + scratch.second);
  stopCq = seekKey->getColQualifier();
  //stopCq.insert(stopCq.end(), scratch.first, scratch.first + scratch.second);

  stopCv = seekKey->getColVisibility();
  //stopCv.insert(stopCv.end(), scratch.first, scratch.first + scratch.second);

  uint64_t timestamp = 0;
  uint64_t prevTimestamp = 0;
  bool previousDeleted = false;

  int rowCmp = -1, cfCmp = -1, cqCmp = -1, cvCmp = -1;
  if (NULL != currKey) {

    prevRow = currKey->getRow();
   // prevRow.insert(prevRow.end(), scratch.first, scratch.first + scratch.second);

    prevCf = currKey->getColFamily();
    //prevCf.insert(prevCf.end(), scratch.first, scratch.first + scratch.second);

    prevCq = currKey->getColQualifier();
    //prevCq.insert(prevCq.end(), scratch.first, scratch.first + scratch.second);

    prevVis = currKey->getColVisibility();
    //prevVis.insert(prevVis.end(), scratch.first, scratch.first + scratch.second);

    prevTimestamp = currKey->getTimeStamp();

    row = currKey->getRow();
    //row.insert(prevRow.end(), scratch.first, scratch.first + scratch.second);

    cf = currKey->getColFamily();
    //cf.insert(prevCf.end(), scratch.first, scratch.first + scratch.second);

    cq = currKey->getColQualifier();
    //cq.insert(prevCq.end(), scratch.first, scratch.first + scratch.second);

    cv = currKey->getColVisibility();
   // cv.insert(prevVis.end(), scratch.first, scratch.first + scratch.second);

    timestamp = currKey->getTimeStamp();

    if (row >= stopRow) {
      if (row > stopRow) {
        (currKey, std::make_shared<Key>(currKey));
        skipped = 0;
        this->prevKey = prevKey;

        return false;
      }

      if (cf >= stopCf) {
        if (cf > stopCf) {
          rkey = std::make_shared<RelativeKey>(currKey, std::make_shared<Key>(currKey), allocatorInstance);
          skipped = 0;
          this->prevKey = prevKey;

          return false;
        }

        if (cq > stopCq) {
          rkey = std::make_shared<RelativeKey>(currKey, std::make_shared<Key>(currKey), allocatorInstance);
          skipped = 0;
          this->prevKey = prevKey;

          return false;
        }
      }
    }

  } else {
    char fieldsSame = -1;

    size_t count = 0;

    std::shared_ptr<Key> newPrevKey = NULL;
    int fieldsPrefixed=0;
    while (count < entriesRemaining) {

      previousDeleted = (fieldsSame & RelativeKey::DELETED) == RelativeKey::DELETED;

      fieldsSame = stream->readByte();

      if ((fieldsSame & PREFIX_COMPRESSION_ENABLED) == PREFIX_COMPRESSION_ENABLED) {
        fieldsPrefixed = stream->readByte();
      } else {
        fieldsPrefixed = 0;
      }

      bool changed = false;

      changed = readPrefix(stream, &rowCmp, RelativeKey::ROW_SAME, RelativeKey::ROW_PREFIX, fieldsSame,fieldsPrefixed, &row, &prevRow, &stopRow);

      if (readPrefix(stream, &cfCmp, RelativeKey::CF_SAME, RelativeKey::CF_PREFIX, fieldsSame,fieldsPrefixed, &cf, &prevCf, &stopCf)) {
        changed = true;
      }

      if (readPrefix(stream, &cqCmp, RelativeKey::CQ_SAME, RelativeKey::CQ_PREFIX, fieldsSame,fieldsPrefixed, &cq, &prevCq, &stopCq)) {
        changed = true;
      }

      if (readPrefix(stream, &cvCmp, RelativeKey::CV_SAME, RelativeKey::CV_PREFIX, fieldsSame,fieldsPrefixed, &cv, &prevVis, &stopCv)) {
        changed = true;
      }

      if ((fieldsSame & RelativeKey::TS_SAME) != RelativeKey::TS_SAME) {
        prevTimestamp = timestamp;
        if ((  fieldsPrefixed & RelativeKey::TS_DIFF) == RelativeKey::TS_DIFF) {
          timestamp = stream->readEncodedVLong() + prevTimestamp;
        } else {
          timestamp = stream->readEncodedVLong();
        }
      }
      try{
        readValue(stream, valCopy);
      }
      catch(...){
        logging::LOG_TRACE(logger) << "Exception reading the " << count << "th of "  << entriesRemaining;
        throw;
      }

      count++;

      if (changed && rowCmp >= 0) {
        if (rowCmp > 0)
          break;

        if (cfCmp >= 0) {
          if (cfCmp > 0)
            break;

          if (cqCmp >= 0)
            break;
        }
      }
      count++;
    }

    if (count > 1) {
      TextBuffer *rowPtr, *cfPtr, *cqPtr, *cvPtr;
      rowPtr = (fieldsSame & RelativeKey::ROW_SAME) == RelativeKey::ROW_SAME ? &row : &prevRow;
      cfPtr = (fieldsSame & RelativeKey::CF_SAME) == RelativeKey::CF_SAME ? &cf : &prevCf;
      cqPtr = (fieldsSame & RelativeKey::CQ_SAME) == RelativeKey::CQ_SAME ? &cq : &prevCq;
      cvPtr = (fieldsSame & RelativeKey::CV_SAME) == RelativeKey::CV_SAME ? &cv : &prevVis;
      long returnTs = (fieldsSame & RelativeKey::TS_SAME) == RelativeKey::TS_SAME ? timestamp : prevTimestamp;
      newPrevKey = allocatorInstance->newKey();
       if (rowPtr->size > 0)
          newPrevKey->setRow(rowPtr->buffer, rowPtr->size, rowPtr->max, false);
        if (cfPtr->size > 0)
          newPrevKey->setColFamily(cfPtr->buffer, cfPtr->size, cfPtr->max, false);
        if (cqPtr->size > 0)
          newPrevKey->setColQualifier(cqPtr->buffer, cqPtr->size, cqPtr->max, false);
        if (cvPtr->size > 0)
          newPrevKey->setColVisibility(cvPtr->buffer, cvPtr->size,cvPtr->max, false);
      newPrevKey->setTimeStamp(returnTs);
      newPrevKey->setDeleted(previousDeleted);
      logging::LOG_TRACE(logger) << "Ended at count " << count << " newPrevKey is " << newPrevKey;

    } else if (count == 1) {
      if (currKey != NULL) {
        newPrevKey = currKey;
      } else {
        newPrevKey = prevKey;
      }
    } else {
      throw std::runtime_error("Illegal state");
    }

    prevKey = newPrevKey;
    rkey = std::make_shared<RelativeKey>(allocatorInstance);
    std::shared_ptr<Key> baseKey = allocatorInstance->newKey();
    if (row.size > 0)
    baseKey->setRow(row.buffer, row.size, row.max,true);
    if (cf.size > 0)
    baseKey->setColFamily(cf.buffer, cf.size, cf.max, true);
    if (cq.size > 0)
    baseKey->setColQualifier(cq.buffer, cq.size, cq.max, true);
    if ( cv.size > 0)
    baseKey->setColVisibility(cv.buffer, cv.size, cv.max, true);
    baseKey->setTimeStamp(timestamp);
    baseKey->setDeleted((fieldsSame & RelativeKey::DELETED) != 0);
    rkey->setBase(baseKey);
    rkey->setPrevious(baseKey);
    skipped = count;

    if (!auths.empty()) {
      cclient::data::security::VisibilityEvaluator eval(auths);
      if (rkey && !eval.evaluate(std::string(cv.buffer,cv.size))) {
        return true;
      }
    }
  }
  return false;
}

}

}
