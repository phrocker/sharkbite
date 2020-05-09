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

#include "data/exceptions/IllegalArgumentException.h"
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
   * don't allow the copy constructor
   */
  explicit Range(const Range &range) = delete;

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
  explicit Range(const std::string &startRow, bool startInclusive, const std::string &endRow, bool endKeyInclusive, bool update = true);

  /**
   * Sets the start key and endkey with the inclusive flags.
   * @param startKey start key
   * @param startInclusive start key is inclusive
   * @param endKey end key
   * @param endKeyInclusive return whether or not the end key is inclusive.
   **/
  explicit Range(const std::shared_ptr<Key> &startKey, bool startInclusive, const std::shared_ptr<Key> &endKey, bool endKeyInclusive, bool update = true);

  /**
   * Sets the start key and endkey with the inclusive flags.
   * @param startKey start key
   * @param startInclusive start key is inclusive
   * @param endKey end key
   * @param endKeyInclusive return whether or not the end key is inclusive.
   **/
  explicit Range(const std::shared_ptr<Key> &endKey, bool endKeyInclusive)
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
  explicit Range(bool startInclusive, const std::shared_ptr<Key> &startKey)
      :
      Range(startKey, startInclusive, 0, false) {
  }

  Range(Range &&) = default;

  /**
   * Returns the start key.
   **/
  std::shared_ptr<Key> getStartKey() const{
    return start;
  }

  /**
   * Returns the end key.
   **/
  std::shared_ptr<Key> getStopKey() const{
    return stop;
  }

  /**
   * returns start key inclusive flag.
   **/
  bool getStartKeyInclusive() const{
    return startKeyInclusive;
  }

  /**
   * returns end key inclusive flag.
   **/
  bool getStopKeyInclusive() const{
    return stopKeyInclusive;
  }

  /**
   * Returns whether or not the start key is infinite.
   **/
  bool getInfiniteStartKey() const{
    return infiniteStartKey;
  }

  /**
   * Returns whether or not the end key is infinite.
   **/
  bool getInfiniteStopKey() const{
    return infiniteStopKey;
  }

  bool afterEndKey(const std::shared_ptr<Key> &key) const;

  bool beforeStartKey(const std::shared_ptr<Key> &key) const;


  std::shared_ptr<Range> intersect(const std::shared_ptr<Range> &range) const;

  virtual ~Range();

  friend inline std::ostream&
  operator <<(std::ostream &out, const Range &rhs) {
    out << "Range ";
    if (rhs.infiniteStartKey || rhs.start == nullptr) {
      out << "(-inf";
    } else {
      out << (rhs.startKeyInclusive ? "[" : "(") << rhs.start;
    }
    out << ",";
    if (rhs.infiniteStopKey || rhs.stop == nullptr) {
      out << "+inf) ";
    } else {
      out << rhs.stop << (rhs.stopKeyInclusive ? "] " : ") ");
    }

    return out;
  }

  friend inline std::ostream&
  operator <<(std::ostream &out, Range *rhs) {
    if (nullptr == rhs)
      return out;
    return operator<<(out, *rhs);
  }

  bool operator==(const Range &other) {
    return start == other.start && stop == other.stop && startKeyInclusive == other.startKeyInclusive && stopKeyInclusive == other.stopKeyInclusive && infiniteStartKey == other.infiniteStartKey
        && infiniteStopKey == other.infiniteStopKey;
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
