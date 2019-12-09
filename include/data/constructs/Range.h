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

#ifndef RANGE_H_
#define RANGE_H_

#include "../exceptions/IllegalArgumentException.h"
#include "Key.h"
#include <memory>
namespace cclient {
namespace data {

/**
 * Range object that contains a start and end key for connecting to accumulo 
 **/
class Range {
 public:
  /**
   * Constructor that sets a null start and end key. ( -inf , +inf )
   **/
  Range();

  /**
     * Sets the range to a single row
     * @param row
     **/
  explicit Range(const std::string &row);

  /**
     * Sets the start key and endkey with the inclusive flags.
     * @param startKey start key
     * @param startInclusive start key is inclusive
     * @param endKey end key
     * @param endKeyInclusive return whether or not the end key is inclusive.
     **/
    explicit Range(const std::string & startRow, bool startInclusive, const std::string &endRow, bool endKeyInclusive);

  /**
   * Sets the start key and endkey with the inclusive flags.
   * @param startKey start key
   * @param startInclusive start key is inclusive
   * @param endKey end key
   * @param endKeyInclusive return whether or not the end key is inclusive.
   **/
  explicit Range(std::shared_ptr<Key> startKey, bool startInclusive, std::shared_ptr<Key> endKey, bool endKeyInclusive);

  /**
   * Sets the start key and endkey with the inclusive flags.
   * @param startKey start key
   * @param startInclusive start key is inclusive
   * @param endKey end key
   * @param endKeyInclusive return whether or not the end key is inclusive.
   **/
  explicit Range(std::shared_ptr<Key> endKey, bool endKeyInclusive)
      :
      Range(0, false, endKey, endKeyInclusive) {
  }

  /**
   * Sets the start key and endkey with the inclusive flags.
   * @param startKey start key
   * @param startInclusive start key is inclusive
   * @param endKey end key
   * @param endKeyInclusive return whether or not the end key is inclusive.
   **/
  explicit Range(bool startInclusive, std::shared_ptr<Key> startKey)
      :
      Range(startKey, startInclusive, 0, false) {
  }

  /**
   * Returns the start key.
   **/
  std::shared_ptr<Key> getStartKey() {
    return start;
  }

  /**
   * Returns the end key.
   **/
  std::shared_ptr<Key> getStopKey() {
    return stop;
  }

  /**
   * returns start key inclusive flag.
   **/
  bool getStartKeyInclusive() {
    return startKeyInclusive;
  }

  /**
   * returns end key inclusive flag.
   **/
  bool getStopKeyInclusive() {
    return stopKeyInclusive;
  }

  /**
   * Returns whether or not the start key is infinite.
   **/
  bool getInfiniteStartKey() {
    return infiniteStartKey;
  }

  /**
   * Returns whether or not the end key is infinite.
   **/
  bool getInfiniteStopKey() {
    return infiniteStopKey;
  }

  bool afterEndKey(std::shared_ptr<Key> key) const {
    if (infiniteStartKey)
      return false;

    if (stopKeyInclusive){
      return stop < key;
    }

    return stop <= key;

  }

  virtual ~Range();

  friend inline std::ostream&
  operator <<(std::ostream &out, Range &rhs) {
    out << "Range ";
    if (rhs.infiniteStartKey) {
      out << "(-inf";
    } else {
      out << (rhs.startKeyInclusive ? "[" : "(") << rhs.start;
    }
    out << ",";
    if (rhs.infiniteStopKey) {
      out << "+inf) ";
    } else {
      out << rhs.stop << (rhs.stopKeyInclusive ? "] " : ") ");
    }

    return out;
  }

  friend inline std::ostream&
  operator <<(std::ostream &out, Range *rhs) {
    return operator<<(out, *rhs);
  }

 protected:
  std::shared_ptr<Key> start;
  std::shared_ptr<Key> stop;
  bool startKeyInclusive;
  bool stopKeyInclusive;
  bool infiniteStartKey;
  bool infiniteStopKey;
};

} /* namespace data */
} /* namespace cclient */
#endif /* RANGE_H_ */
