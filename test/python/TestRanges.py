from testmodule import *
import sys

class TestWrites(TestRunner):
		
	def __init__(self):
		super().__init__()
	
	def mthd(self):
	
		import pysharkbite
		
		tableOperations = super().getTableOperations()
			
		if not tableOperations.exists(False):
		    print ("Creating table")
		    tableOperations.create(False)  
		else:
		    print ("Table already exists, so not creating it")  
		
		
		auths = pysharkbite.Authorizations()
		auths.addAuthorization("blah1")
		auths.addAuthorization("blah2")
		
		securityOps = super().getConnector().securityOps()
		
		securityOps.grantAuthorizations(auths,"root")
		
		""" Add authorizations """ 
		""" mutation.put("cf","cq","cv",1569786960) """
		
		writer = tableOperations.createWriter(auths, 10)
		
		mutation = pysharkbite.Mutation("row2");    
		
		mutation.put("cf","cq","blah1",1569786960, "value")
		mutation.put("cf2","cq2","blah1",1569786960, "value2")
		""" no value """
		mutation.put("cf3","cq3","blah2",1569786960, "") 
		
		writer.addMutation( mutation )
		
		writer.close()
		
		print("written")
		
		auths = pysharkbite.Authorizations()
		
		auths.addAuthorization("blah1")
		
		scanner = tableOperations.createScanner(auths, 2)
		
		startKey = pysharkbite.Key()
		
		endKey = pysharkbite.Key()
		
		startKey.setRow("row")
		
		endKey.setRow("row3")
		
		# test single range
		range = pysharkbite.Range("row2")
		
		scanner.addRange( range )
		
		resultset = scanner.getResultSet()
		
		for keyvalue in resultset:
			key = keyvalue.getKey()
			assert( "row2" == key.getRow() )
			value = keyvalue.getValue()
			if "cf" == key.getColumnFamily():
				assert( "value"  == value.get() )
			if ("cf2" == key.getColumnFamily() ):
				assert( "value2" == value.get() )
			if ("cf3" == key.getColumnFamily() ):
				print("Unexpected column cf3")
				sys.exit(1)
		    
		range = pysharkbite.Range("row1",True,"row1.5",True)
		
		scanner.addRange( range )
		
		resultset = scanner.getResultSet()
		
		for keyvalue in resultset:
			print("Unexpected result")
			sys.exit(1)
		    
		# test single range
		range = pysharkbite.Range("row",False,"row3",True)
		
		scanner = tableOperations.createScanner(auths, 2)
		
		scanner.addRange( range )
		
		resultset = scanner.getResultSet()
		
		count =0
		for keyvalue in resultset:
			key = keyvalue.getKey()
			assert( "row2" == key.getRow() )
			value = keyvalue.getValue()
			if "cf" == key.getColumnFamily():
				assert( "value"  == value.get() )
			if ("cf2" == key.getColumnFamily() ):
				assert( "value2" == value.get() )
			if ("cf3" == key.getColumnFamily() ):
				print("Unexpected column cf3")
				sys.exit(1)
			count=count+1
		if count <= 0:
			print("Expected results")
			sys.exit(1)
			
		
		# test infinite range
		range = pysharkbite.Range("",False,"row3",True)
		
		scanner = tableOperations.createScanner(auths, 2)
		
		scanner.addRange( range )
		
		resultset = scanner.getResultSet()
		
		count =0
		for keyvalue in resultset:
			key = keyvalue.getKey()
			assert( "row2" == key.getRow() )
			value = keyvalue.getValue()
			if "cf" == key.getColumnFamily():
				assert( "value"  == value.get() )
			if ("cf2" == key.getColumnFamily() ):
				assert( "value2" == value.get() )
			if ("cf3" == key.getColumnFamily() ):
				print("Unexpected column cf3")
				sys.exit(1)
			count=count+1
		if count <= 0:
			print("Expected results")
			sys.exit(1)
		
		""" delete your table if user did not create temp """
		
		tableOperations.remove()


runner = TestWrites()
runner.mthd()