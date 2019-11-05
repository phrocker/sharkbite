#!/bin/bash

INDIR=$1
OUTDIR=$2

ls ${INDIR}/*.thrift | xargs -I {} ./thrift --gen cpp -o out {}
rm ${OUTDIR}/gen-cpp/*skeleton*
grep -l stdcxx ${OUTDIR}/gen-cpp/* | xargs -I {} sed -i '' 's/::apache::thrift::stdcxx::/std::/g' {}
grep -l stdcxx ${OUTDIR}/gen-cpp/* | xargs -I {} sed -i '' 's/apache::thrift::stdcxx::/std::/g' {}
