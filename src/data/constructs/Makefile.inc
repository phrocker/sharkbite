# put common definitions in here
CC	= g++
PRJCFLAGS	= -g --std=c++11 -lthrift -I/usr/local/include/thrift/  -shared -fPIC
LD	= g++
LDFLAGS	= -lthrift -I/usr/local/include/thrift/  -shared -fPIC
AR	= ar
ARFLAGS	=
RANLIB	= ranlib
RM	= rm
ECHO	= echo

SHELL	= /bin/sh

.SILENT :
