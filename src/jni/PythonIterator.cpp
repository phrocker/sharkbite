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

#include "jni/PythonIterator.h"

#include "jni/AccumuloRange.h"
#include "jni/DSLIterator.h"
#include "jni/Iterators.h"
#include "jni/JNIUtil.h"
#include "jni/JVMLoader.h"

namespace cclient {
namespace jni {
namespace python {
/*
void PythonIterator::callNext() {
  topKeyValue = iter.callNext(this);
  if (nullptr != topKeyValue) {
    topKey = topKeyValue->getKey();
    topValue = topKeyValue->getValue();
  } else {
    topKey = nullptr;
    topValue = nullptr;
    calledNext = true;
    return;
  }
  // if the user did not called next then we should call the iterator's next
  if (!calledNext) {
    next();
  }
  calledNext = false;
}

void PythonIterator::callSeek(const std::shared_ptr<cclient::data::Range>
&range) { calledNext = false; if (iter.callSeek(this, range)) { callNext(); }
else { accIter->seek(range); callNext();
  }
}

void PythonIterator::setDSL(const std::string &dsl) {
  iter.eval(dsl);
  calledNext = false;
}

std::shared_ptr<cclient::data::Key> PythonIterator::getTopKey() {
  return topKey;
}
std::shared_ptr<cclient::data::Value> PythonIterator::getTopValue() {
  if (nullptr == topValue) {
    topValue = std::make_shared<cclient::data::Value>();
  }
  return topValue;
}

bool PythonIterator::hasTop() {
  return (nullptr != topKey);
}

void PythonIterator::next() {
  calledNext = true;
}
*/

}
}  // namespace jni
}  // namespace cclient
