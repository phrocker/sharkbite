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

#ifndef CLIENTEXCEPTION_H_
#define CLIENTEXCEPTION_H_

#include <map>
#include <string>

#define INVALID_ZK_DATA 0
#define INVALID_ZK_SERVER_DATA 1
#define INVALID_ZK_SERVER_PORT 2
#define INVALID_SERVER_PORT 3
#define NO_MASTER_FOUND 4
#define INVALID_USERNAME_PASSWORD 5
#define COULD_NOT_CREATE_NAMESPACE 6
#define DELETE_DEFAULT_NAMESPACE 7
#define NO_LOCATION_IDENTIFIED 8
#define TABLE_NOT_FOUND 9
#define RANGE_NOT_SPECIFIED 10
#define TABLE_OR_NAMESPACE_EMPTY 11
#define ARGUMENT_CANNOT_BE_NULL 12
#define SCANNER_ALREADY_STARTED 13

namespace cclient {
namespace exceptions {

extern const std::map<uint16_t, std::string> CLIENT_ERROR_CODES;

class ClientException : public std::exception {
 public:
  explicit ClientException(std::string excp) : errorCode(-1), excp_str(excp) {}

  ClientException(const uint16_t errorCode) : errorCode(errorCode) {}

  ~ClientException() throw() {}
  const char* what() const throw() {
    if (errorCode != -1) {
      return CLIENT_ERROR_CODES.at(errorCode).c_str();
    } else
      return excp_str.c_str();
  }
  const int getErrorCode() const { return errorCode; }

 private:
  int16_t errorCode;
  std::string excp_str;
};
}  // namespace exceptions
} /* namespace cclient */
#endif /* CLIENTEXCEPTION_H_ */
