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

#ifndef HDFSEXCEPTION_H_
#define HDFSEXCEPTION_H_

#include <map>
#include <string>

namespace cclient {
namespace exceptions {

class HDFSException : public std::exception {
 public:
  explicit HDFSException(std::string excp) : excp_str(excp) {}

  ~HDFSException() throw() {}
  const char* what() const throw() { return excp_str.c_str(); }

 private:
  std::string excp_str;
};
}  // namespace exceptions
} /* namespace cclient */
#endif /* HDFSEXCEPTION_H_ */
