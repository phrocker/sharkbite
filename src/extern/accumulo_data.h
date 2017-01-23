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
#ifndef SRC_EXTERN_ACCUMULO_DATA_H_
#define SRC_EXTERN_ACCUMULO_DATA_H_

struct Key
{
  /**
   * Row part of key
   */
  uint8_t *row;
  uint32_t rowMaxSize;
  uint32_t rowLength;

  /**
   * Column family
   */
  uint32_t columnFamilyLength;
  uint8_t *colFamily;
  uint32_t columnFamilySize;

  /**
   * Column qualifier.
   */
  uint8_t *colQualifier;
  uint32_t colQualSize;
  uint32_t colQualLen;

  uint8_t *keyVisibility;
  uint32_t colVisSize;
  uint64_t timestamp;

  bool deleted;

};

struct Value
{
  // value array.
  uint8_t *value;
  // offset;
  uint16_t offset;
  // value size.
  size_t valueSize;
};

struct KeyValue
{
  struct Key *key;
  struct Value *value;
};

struct KeyValueList
{
  struct KeyValue **kvs;
  uint32_t kv_size;
};

struct Range
{
  struct Key *start;
  struct Key *stop;
  bool startKeyInclusive;
  bool stopKeyInclusive;
  bool infiniteStartKey;
  bool infiniteStopKey;
};

#endif /* SRC_EXTERN_ACCUMULO_DATA_H_ */
