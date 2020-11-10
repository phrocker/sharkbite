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

#ifndef INCLUDE_JNI_DSLITERATOR_H_
#define INCLUDE_JNI_DSLITERATOR_H_

#include <jni.h>
#include "data/constructs/Key.h"
#include "data/constructs/value.h"
#include "JNIUtil.h"

#ifdef __cplusplus
extern "C" {
#endif

void rethrow_cpp_exception_as_java_exception(JNIEnv*);

JNIEXPORT void JNICALL Java_org_apache_accumulo_tserver_tablet_NativeCompactor_callCompact(JNIEnv* env, jobject me, jstring output_file, jobject arraylist_files, jobject longadder_resulted, jobject longadder_total, jobject longadder_filesize, jlong ageoff);

#ifdef __cplusplus
}
#endif

#endif /* INCLUDE_JNI_DSLITERATOR_H_ */
